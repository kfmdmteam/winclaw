//go:build windows

// WinClaw — Windows-native terminal AI assistant.
// Build: go build -ldflags="-s -w" -o winclaw.exe ./cmd/winclaw
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows/svc"

	"winclaw/internal/agent"
	"winclaw/internal/api"
	"winclaw/internal/config"
	"winclaw/internal/db"
	"winclaw/internal/ipc"
	"winclaw/internal/logging"
	"winclaw/internal/memory"
	"winclaw/internal/scheduler"
	"winclaw/internal/security"
	svcpkg "winclaw/internal/service"
	"winclaw/internal/terminal"
	"winclaw/internal/tools"
)

const (
	version    = "v0.1.0"
	apiKeyName = "AnthropicAPIKey"
)

func main() {
	// ── Windows Service auto-detection ────────────────────────────────────────
	// When the SCM launches us, svc.IsWindowsService returns true. In that case
	// we skip the REPL entirely and hand control to the service handler.
	if isService, err := svc.IsWindowsService(); err == nil && isService {
		if runErr := svcpkg.Run(); runErr != nil {
			os.Exit(1)
		}
		return
	}

	// ── Flags ────────────────────────────────────────────────────────────────
	flagVersion         := flag.Bool("version", false, "print version and exit")
	flagSetup           := flag.Bool("setup", false, "run first-time setup wizard")
	flagSession         := flag.String("session", "", "session ID to resume")
	flagModel           := flag.String("model", "", "override the model (e.g. claude-opus-4-6)")
	flagNoColor         := flag.Bool("no-color", false, "disable ANSI colour output")
	flagLogLevel        := flag.String("log-level", "", "log level: debug, info, warning, error")
	flagInstallService  := flag.Bool("install-service", false, "register WinClaw as a Windows Service (requires administrator)")
	flagUninstallService := flag.Bool("uninstall-service", false, "remove the WinClaw Windows Service (requires administrator)")
	flagStartService    := flag.Bool("start-service", false, "start the WinClaw service via SCM")
	flagStopService     := flag.Bool("stop-service", false, "stop the WinClaw service via SCM")
	flagServiceStatus   := flag.Bool("service-status", false, "print the service status and exit")
	flagSend            := flag.String("send", "", "send a prompt to the running service and stream the response")
	flag.Parse()

	// ── Version ──────────────────────────────────────────────────────────────
	if *flagVersion {
		fmt.Printf("WinClaw %s\n", version)
		os.Exit(0)
	}

	// ── Load config ──────────────────────────────────────────────────────────
	cfg, err := config.Load()
	if err != nil {
		fatalf("load config: %v", err)
	}

	// Apply flag overrides.
	if *flagModel != "" {
		cfg.Model = *flagModel
	}
	if *flagLogLevel != "" {
		cfg.LogLevel = *flagLogLevel
	}
	// noColor is passed directly to the REPL, not stored in cfg.
	noColor := *flagNoColor

	// ── Setup wizard ─────────────────────────────────────────────────────────
	if *flagSetup {
		runSetup(cfg)
		os.Exit(0)
	}

	// ── Service status (no credentials required) ──────────────────────────────
	if *flagServiceStatus {
		fmt.Printf("WinClaw service: %s\n", svcpkg.Status())
		os.Exit(0)
	}

	// ── Service stop (no API key required) ────────────────────────────────────
	if *flagStopService {
		if err := svcpkg.Stop(); err != nil {
			fatalf("stop service: %v", err)
		}
		fmt.Println("WinClaw service: stop signal sent.")
		os.Exit(0)
	}

	// ── Service uninstall ─────────────────────────────────────────────────────
	if *flagUninstallService {
		if err := svcpkg.Uninstall(); err != nil {
			fatalf("uninstall service: %v", err)
		}
		fmt.Println("WinClaw service uninstalled.")
		os.Exit(0)
	}

	// ── Send prompt to running service ────────────────────────────────────────
	if *flagSend != "" {
		sessionID := *flagSession
		prompt := strings.TrimSpace(*flagSend)
		if prompt == "" {
			fatalf("--send requires a non-empty prompt")
		}
		chunks, err := ipc.Send(sessionID, prompt)
		if err != nil {
			fatalf("send: %v", err)
		}
		for c := range chunks {
			if c.Tool != "" {
				fmt.Printf("\n  ▸ %s\n", c.Tool)
				continue
			}
			if c.Text != "" {
				fmt.Print(c.Text)
			}
			if c.Done {
				fmt.Println()
				if c.Error != "" {
					fatalf("service error: %s", c.Error)
				}
			}
		}
		os.Exit(0)
	}

	// ── Service start ─────────────────────────────────────────────────────────
	if *flagStartService {
		if err := svcpkg.Start(); err != nil {
			fatalf("start service: %v", err)
		}
		fmt.Println("WinClaw service: started.")
		os.Exit(0)
	}

	// ── Read API key from Credential Manager ─────────────────────────────────
	apiKey, err := security.ReadSecret(apiKeyName)
	if err != nil {
		fmt.Fprintln(os.Stderr, "WinClaw: API key not found in Credential Manager.")
		fmt.Fprintln(os.Stderr, "Run with --setup to configure WinClaw.")
		os.Exit(1)
	}
	if len(apiKey) == 0 {
		fmt.Fprintln(os.Stderr, "WinClaw: stored API key is empty.")
		fmt.Fprintln(os.Stderr, "Run with --setup to reconfigure.")
		os.Exit(1)
	}

	// ── Apply ACLs to data directory ─────────────────────────────────────────
	if err := os.MkdirAll(cfg.DataDir, 0700); err != nil {
		fatalf("create data dir: %v", err)
	}
	if err := security.LockDirToCurrentUser(cfg.DataDir); err != nil {
		fatalf("could not apply ACLs to data directory %s: %v\n"+
			"WinClaw refuses to start with an unlocked data directory.\n"+
			"Run 'winclaw.exe --setup' to reinitialise, or check that your\n"+
			"account has the SeSecurityPrivilege right.", cfg.DataDir, err)
	}

	// ── Install service ───────────────────────────────────────────────────────
	if *flagInstallService {
		runInstallService(cfg, apiKey)
		os.Exit(0)
	}

	// ── Open database ─────────────────────────────────────────────────────────
	database, err := db.Open(cfg.DataDir)
	if err != nil {
		fatalf("open database: %v", err)
	}
	defer database.Close()

	// ── Event log ────────────────────────────────────────────────────────────
	evtLog, err := logging.Open()
	if err != nil {
		// logging.Open always succeeds (falls back to stderr), but handle
		// the hypothetical error defensively.
		fmt.Fprintf(os.Stderr, "warning: event log open: %v\n", err)
	}
	defer evtLog.Close()

	// ── Audit: startup ────────────────────────────────────────────────────────
	evtLog.Security("startup", "winclaw", fmt.Sprintf("version=%s", version))

	// ── Memory manager ────────────────────────────────────────────────────────
	memMgr, err := memory.NewMemoryManager(cfg.DataDir)
	if err != nil {
		fatalf("create memory manager: %v", err)
	}

	// ── Soul file ─────────────────────────────────────────────────────────────
	if err := memMgr.InitSoul(); err != nil {
		fatalf("init soul file: %v", err)
	}

	// ── Session manager ───────────────────────────────────────────────────────
	sessionMgr := agent.NewSessionManager(database.Conn(), memMgr)

	// ── Resolve or create the active session ──────────────────────────────────
	var sess *agent.Session
	if *flagSession != "" {
		sess, err = sessionMgr.Load(*flagSession)
		if err != nil {
			fatalf("load session %q: %v", *flagSession, err)
		}
	} else {
		// Load the most-recent session, or create a default one.
		sessions, listErr := sessionMgr.List()
		if listErr != nil {
			fatalf("list sessions: %v", listErr)
		}
		if len(sessions) > 0 {
			sess = sessions[0]
		} else {
			sess, err = sessionMgr.Create("default")
			if err != nil {
				fatalf("create default session: %v", err)
			}
		}
	}

	// ── API client ────────────────────────────────────────────────────────────
	apiClient := api.NewClient(apiKey, cfg.Model)
	defer apiClient.Close() // zeros the key from heap memory on exit

	// ── Tools ─────────────────────────────────────────────────────────────────
	toolRegistry := tools.NewRegistry(
		func(content string) error { return memMgr.Append(sess.ID, content) },
		func(content string) error { return memMgr.WriteSoul(content) },
	)

	// ── Agent ─────────────────────────────────────────────────────────────────
	ag := agent.NewAgent(sess, apiClient, memMgr, cfg, toolRegistry, nil)

	// ── Scheduler ────────────────────────────────────────────────────────────
	sched := scheduler.NewScheduler(database.Conn(), func(ctx context.Context, sessionID, prompt string) error {
		scheduledSess, loadErr := sessionMgr.Load(sessionID)
		if loadErr != nil {
			return fmt.Errorf("scheduler: load session %q: %w", sessionID, loadErr)
		}
		schedTools := tools.NewRegistry(
			func(content string) error { return memMgr.Append(scheduledSess.ID, content) },
			func(content string) error { return memMgr.WriteSoul(content) },
		)
		scheduledAgent := agent.NewAgent(scheduledSess, apiClient, memMgr, cfg, schedTools, nil)
		_, runErr := scheduledAgent.Run(ctx, prompt)
		return runErr
	})

	// ── Root context with signal handling ─────────────────────────────────────
	rootCtx, rootCancel := context.WithCancel(context.Background())
	defer rootCancel()

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt)
	go func() {
		<-sigCh
		rootCancel()
	}()

	// ── Scheduler background goroutine ────────────────────────────────────────
	go sched.Start(rootCtx)

	// ── REPL ──────────────────────────────────────────────────────────────────
	toolsMaker := func(s *agent.Session) *tools.Registry {
		return tools.NewRegistry(
			func(content string) error { return memMgr.Append(s.ID, content) },
			func(content string) error { return memMgr.WriteSoul(content) },
		)
	}
	repl := terminal.NewREPL(cfg, sess, ag, sessionMgr, memMgr, sched, noColor, toolsMaker, version)

	if runErr := repl.Run(rootCtx); runErr != nil {
		// Non-zero exit for unexpected errors; EOF / user exit is normal.
		fmt.Fprintf(os.Stderr, "repl: %v\n", runErr)
	}

	// ── Shutdown ──────────────────────────────────────────────────────────────
	evtLog.Security("shutdown", "winclaw", fmt.Sprintf("version=%s", version))
	sched.Stop()
}

// ─────────────────────────────────────────────────────────────────────────────
// Service installation
// ─────────────────────────────────────────────────────────────────────────────

// runInstallService registers WinClaw as a Windows Service and prepares the
// machine-DPAPI-encrypted key file that the service reads at runtime.
// Must be run as administrator (CreateService requires it).
func runInstallService(cfg *config.Config, apiKey []byte) {
	exePath, err := os.Executable()
	if err != nil {
		fatalf("resolve executable path: %v", err)
	}
	exePath, err = filepath.Abs(exePath)
	if err != nil {
		fatalf("make path absolute: %v", err)
	}

	// Warn if the binary is in a user-writable location.
	lower := strings.ToLower(exePath)
	if strings.Contains(lower, "\\temp\\") ||
		strings.Contains(lower, "\\downloads\\") ||
		strings.Contains(lower, "\\appdata\\local\\temp\\") {
		fmt.Fprintf(os.Stderr, "WARNING: binary is in a temporary location (%s).\n", exePath)
		fmt.Fprintf(os.Stderr, "         Copy it to a permanent path before continuing.\n")
	}

	fmt.Printf("Installing service for: %s\n", exePath)

	// Get the installing user's SID for pipe and file ACLs.
	userSID, err := security.GetCurrentUserSID()
	if err != nil {
		fatalf("get user SID: %v", err)
	}
	userSIDStr := userSID.String()

	// Re-encrypt the API key with machine DPAPI so LocalService can read it.
	encKey, err := security.EncryptMachine(apiKey)
	if err != nil {
		fatalf("encrypt service key: %v", err)
	}

	keyPath := svcpkg.ServiceKeyPath(cfg.DataDir)
	if err := os.WriteFile(keyPath, encKey, 0600); err != nil {
		fatalf("write service-key.enc: %v", err)
	}

	// Lock service-key.enc to user + LocalService only.
	lsSID, err := security.LocalServiceSID()
	if err != nil {
		fatalf("get LocalService SID: %v", err)
	}
	if err := security.GrantFileToSIDs(keyPath, userSID, lsSID); err != nil {
		fatalf("ACL service-key.enc: %v", err)
	}
	fmt.Printf("Service key written and locked: %s\n", keyPath)

	// Re-apply data directory ACL to include LocalService.
	if err := security.LockDirToService(cfg.DataDir); err != nil {
		fatalf("ACL data directory for service: %v", err)
	}
	fmt.Printf("Data directory ACL updated: %s\n", cfg.DataDir)

	// Store the owner SID in config so the service can set the pipe DACL.
	cfg.ServiceOwnerSID = userSIDStr
	if err := cfg.Save(); err != nil {
		fatalf("save config: %v", err)
	}
	fmt.Printf("Config saved with ServiceOwnerSID: %s\n", userSIDStr)

	// Register the Windows Service.
	if err := svcpkg.Install(exePath); err != nil {
		fatalf("%v", err)
	}

	fmt.Println()
	fmt.Println("Service installed successfully.")
	fmt.Println()
	fmt.Println("To start:        winclaw.exe --start-service")
	fmt.Println("To check status: winclaw.exe --service-status")
	fmt.Println("To send a prompt: winclaw.exe --send \"your prompt here\"")
	fmt.Println("To stop:         winclaw.exe --stop-service")
	fmt.Println("To remove:       winclaw.exe --uninstall-service")
}

// ─────────────────────────────────────────────────────────────────────────────
// Setup wizard
// ─────────────────────────────────────────────────────────────────────────────

// runSetup runs the interactive first-time configuration wizard.
func runSetup(cfg *config.Config) {
	fmt.Println("=== WinClaw Setup ===")
	fmt.Println()

	// Prompt for the API key.
	// Input is visible so that paste works reliably across all Windows
	// terminals. The key goes directly to Credential Manager and is never
	// written to disk.
	fmt.Println("Paste or type your Anthropic API key, then press Enter.")
	fmt.Println("(Input is visible in your terminal but is never saved to disk.)")
	fmt.Print("> ")
	apiKey, err := readLine()
	if err != nil {
		fatalf("read API key: %v", err)
	}
	if apiKey == "" {
		fatalf("API key must not be empty")
	}

	// Store in Credential Manager.
	if err := security.StoreSecret(apiKeyName, apiKey); err != nil {
		fatalf("store API key: %v", err)
	}
	fmt.Println("API key stored in Windows Credential Manager.")

	// Create and lock the data directory.
	if err := os.MkdirAll(cfg.DataDir, 0700); err != nil {
		fatalf("create data dir: %v", err)
	}
	if err := security.LockDirToCurrentUser(cfg.DataDir); err != nil {
		fatalf("apply ACLs to %s: %v", cfg.DataDir, err)
	}
	fmt.Printf("Data directory: %s\n", cfg.DataDir)

	// Write default config.json.
	if err := cfg.Save(); err != nil {
		fatalf("save config: %v", err)
	}
	fmt.Printf("Configuration saved to %s\n", filepath.Join(cfg.DataDir, "config.json"))

	fmt.Println()
	fmt.Println("Setup complete. Run winclaw to start.")
}

// readLine reads a single line from stdin, trimming CR/LF.
// Plain line reading works reliably for paste across all Windows terminals.
func readLine() (string, error) {
	var line strings.Builder
	buf := make([]byte, 1)
	for {
		n, err := os.Stdin.Read(buf)
		if n > 0 {
			c := buf[0]
			if c == '\n' {
				break
			}
			if c != '\r' {
				line.WriteByte(c)
			}
		}
		if err != nil {
			if line.Len() > 0 {
				break // EOF after partial input is fine
			}
			return "", fmt.Errorf("read input: %w", err)
		}
	}
	return strings.TrimSpace(line.String()), nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Audit log helpers
// ─────────────────────────────────────────────────────────────────────────────

// writeAuditLog inserts a row into the audit_log table.  Non-fatal; errors
// are silently discarded so an audit failure never terminates the process.
func writeAuditLog(d *db.DB, action, subject, detail string, success bool) {
	if d == nil {
		return
	}
	successInt := 1
	if !success {
		successInt = 0
	}
	_, _ = d.Conn().Exec(
		`INSERT INTO audit_log (event_time, action, subject, detail, success) VALUES (?, ?, ?, ?, ?)`,
		time.Now().UTC().Unix(), action, subject, detail, successInt,
	)
}

// ─────────────────────────────────────────────────────────────────────────────
// Misc
// ─────────────────────────────────────────────────────────────────────────────

func fatalf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "WinClaw: "+format+"\n", args...)
	os.Exit(1)
}
