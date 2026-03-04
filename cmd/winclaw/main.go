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

	"winclaw/internal/agent"
	"winclaw/internal/api"
	"winclaw/internal/config"
	"winclaw/internal/db"
	"winclaw/internal/logging"
	"winclaw/internal/memory"
	"winclaw/internal/scheduler"
	"winclaw/internal/security"
	"winclaw/internal/terminal"
	"winclaw/internal/tools"
)

const (
	version    = "v0.1.0"
	apiKeyName = "AnthropicAPIKey"
)

func main() {
	// ── Flags ────────────────────────────────────────────────────────────────
	flagVersion  := flag.Bool("version", false, "print version and exit")
	flagSetup    := flag.Bool("setup", false, "run first-time setup wizard")
	flagSession  := flag.String("session", "", "session ID to resume")
	flagModel    := flag.String("model", "", "override the model (e.g. claude-opus-4-6)")
	flagNoColor := flag.Bool("no-color", false, "disable ANSI colour output")
	flagLogLevel := flag.String("log-level", "", "log level: debug, info, warning, error")
	flag.Parse()

	// ── Version ──────────────────────────────────────────────────────────────
	if *flagVersion {
		fmt.Printf("WinClaw %s\n", version)
		os.Exit(0)
	}

	// ── Banner ───────────────────────────────────────────────────────────────
	fmt.Printf("WinClaw %s — Windows-Native Terminal AI Assistant\n", version)

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
	repl := terminal.NewREPL(cfg, sess, ag, sessionMgr, memMgr, sched, noColor, toolsMaker)

	if runErr := repl.Run(rootCtx); runErr != nil {
		// Non-zero exit for unexpected errors; EOF / user exit is normal.
		fmt.Fprintf(os.Stderr, "repl: %v\n", runErr)
	}

	// ── Shutdown ──────────────────────────────────────────────────────────────
	evtLog.Security("shutdown", "winclaw", fmt.Sprintf("version=%s", version))
	sched.Stop()
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
