//go:build windows

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"

	"winclaw/internal/agent"
	"winclaw/internal/api"
	"winclaw/internal/config"
	"winclaw/internal/db"
	"winclaw/internal/ipc"
	"winclaw/internal/memory"
	"winclaw/internal/scheduler"
	"winclaw/internal/security"
	"winclaw/internal/tools"
)

// ServiceKeyFile is the filename of the machine-DPAPI-encrypted API key.
const ServiceKeyFile = "service-key.enc"

// ServiceKeyPath returns the full path to the encrypted service key.
func ServiceKeyPath(dataDir string) string {
	return filepath.Join(dataDir, ServiceKeyFile)
}

// Run is called when the binary is launched by the Windows Service Control
// Manager. It blocks until the service stops.
func Run() error {
	return svc.Run(ServiceName, &winClawService{})
}

type winClawService struct{}

func (ws *winClawService) Execute(
	_ []string,
	requests <-chan svc.ChangeRequest,
	status chan<- svc.Status,
) (bool, uint32) {
	const accepts = svc.AcceptStop | svc.AcceptShutdown

	status <- svc.Status{State: svc.StartPending}

	elog, _ := eventlog.Open(ServiceName)
	if elog != nil {
		defer elog.Close()
	}
	info := func(msg string) {
		if elog != nil {
			_ = elog.Info(1, msg)
		}
	}
	warn := func(msg string) {
		if elog != nil {
			_ = elog.Warning(2, msg)
		}
	}
	fail := func(msg string) {
		if elog != nil {
			_ = elog.Error(3, msg)
		}
		status <- svc.Status{State: svc.Stopped}
	}

	info("WinClaw service: starting")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Config.
	cfg, err := config.Load()
	if err != nil {
		fail(fmt.Sprintf("load config: %v", err))
		return false, 1
	}

	// Decrypt service API key from machine-DPAPI file.
	encKey, err := os.ReadFile(ServiceKeyPath(cfg.DataDir))
	if err != nil {
		fail(fmt.Sprintf("read %s: %v — run winclaw.exe --install-service as administrator", ServiceKeyFile, err))
		return false, 1
	}
	apiKey, err := security.DecryptMachine(encKey)
	if err != nil {
		fail(fmt.Sprintf("decrypt service key: %v", err))
		return false, 1
	}
	defer clear(apiKey)

	// Database.
	database, err := db.Open(cfg.DataDir)
	if err != nil {
		fail(fmt.Sprintf("open database: %v", err))
		return false, 1
	}
	defer database.Close()

	// Memory + soul.
	memMgr, err := memory.NewMemoryManager(cfg.DataDir)
	if err != nil {
		fail(fmt.Sprintf("memory manager: %v", err))
		return false, 1
	}
	_ = memMgr.InitSoul()

	// Session manager + API client.
	sessionMgr := agent.NewSessionManager(database.Conn(), memMgr)
	apiClient := api.NewClient(apiKey, cfg.Model)
	defer apiClient.Close()

	// Scheduler — fires on cron/interval schedules.
	sched := scheduler.NewScheduler(database.Conn(), func(ctx context.Context, sessionID, prompt string) error {
		sess, loadErr := sessionMgr.Load(sessionID)
		if loadErr != nil {
			return fmt.Errorf("scheduler: load session: %w", loadErr)
		}
		tl := tools.NewRegistry(
			func(content string) error { return memMgr.Append(sess.ID, content) },
			func(content string) error { return memMgr.WriteSoul(content) },
			tools.Options{GlobalUpdate: func(c string) error { return memMgr.AppendGlobal(c) }},
		)
		ag := agent.NewAgent(sess, apiClient, memMgr, cfg, tl, nil)
		_, runErr := ag.Run(ctx, prompt)
		return runErr
	})
	go sched.Start(ctx)
	defer sched.Stop()

	// Named pipe server — accepts --send connections.
	if cfg.ServiceOwnerSID == "" {
		warn("ServiceOwnerSID not in config; --send disabled. Re-run --install-service.")
	} else {
		pipeServer, pipeErr := ipc.NewServer(cfg.ServiceOwnerSID)
		if pipeErr != nil {
			warn(fmt.Sprintf("pipe server: %v — --send disabled", pipeErr))
		} else {
			handler := makePipeHandler(ctx, sessionMgr, apiClient, memMgr, cfg)
			go pipeServer.ServeForever(handler)
			defer pipeServer.Close()
		}
	}

	status <- svc.Status{State: svc.Running, Accepts: accepts}
	info("WinClaw service: running")

	for {
		c := <-requests
		switch c.Cmd {
		case svc.Stop, svc.Shutdown:
			info("WinClaw service: stopping")
			status <- svc.Status{State: svc.StopPending}
			cancel()
			return false, 0
		case svc.Interrogate:
			status <- c.CurrentStatus
		}
	}
}

// makePipeHandler returns a function that handles a single pipe client session.
func makePipeHandler(
	ctx context.Context,
	sessionMgr *agent.SessionManager,
	apiClient *api.Client,
	memMgr *memory.MemoryManager,
	cfg *config.Config,
) func(h windows.Handle) {
	return func(h windows.Handle) {
		// Read request.
		data, err := ipc.ReadFrame(h)
		if err != nil {
			_ = ipc.WriteFrame(h, ipc.Chunk{Done: true, Error: fmt.Sprintf("read request: %v", err)})
			return
		}
		var req ipc.Request
		if err := json.Unmarshal(data, &req); err != nil {
			_ = ipc.WriteFrame(h, ipc.Chunk{Done: true, Error: fmt.Sprintf("parse request: %v", err)})
			return
		}

		// Resolve session.
		sess := resolveSession(sessionMgr, req.Session)
		if sess == nil {
			_ = ipc.WriteFrame(h, ipc.Chunk{Done: true, Error: "no session available"})
			return
		}

		// Run agent, streaming output to the pipe.
		tl := tools.NewRegistry(
			func(content string) error { return memMgr.Append(sess.ID, content) },
			func(content string) error { return memMgr.WriteSoul(content) },
			tools.Options{GlobalUpdate: func(c string) error { return memMgr.AppendGlobal(c) }},
		)
		ag := agent.NewAgent(sess, apiClient, memMgr, cfg, tl, func(chunk string) {
			if strings.HasPrefix(chunk, "\x00TOOL\x00") {
				name := strings.TrimPrefix(chunk, "\x00TOOL\x00")
				_ = ipc.WriteFrame(h, ipc.Chunk{Tool: name})
			} else {
				_ = ipc.WriteFrame(h, ipc.Chunk{Text: chunk})
			}
		})

		_, runErr := ag.Run(ctx, req.Prompt)
		errStr := ""
		if runErr != nil {
			errStr = runErr.Error()
		}
		_ = ipc.WriteFrame(h, ipc.Chunk{Done: true, Error: errStr})
	}
}

// resolveSession loads the named session or falls back to the most-recent one.
// Creates "service-default" if no sessions exist.
func resolveSession(mgr *agent.SessionManager, nameOrID string) *agent.Session {
	if nameOrID != "" {
		if sess, err := mgr.Load(nameOrID); err == nil {
			return sess
		}
	}
	sessions, err := mgr.List()
	if err == nil && len(sessions) > 0 {
		return sessions[0]
	}
	sess, err := mgr.Create("service-default")
	if err != nil {
		return nil
	}
	return sess
}
