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
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"winclaw/internal/agent"
	"winclaw/internal/api"
	"winclaw/internal/config"
	"winclaw/internal/db"
	"winclaw/internal/logging"
	"winclaw/internal/memory"
	"winclaw/internal/scheduler"
	"winclaw/internal/security"
	"winclaw/internal/terminal"
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
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "WinClaw: stored API key is empty.")
		fmt.Fprintln(os.Stderr, "Run with --setup to reconfigure.")
		os.Exit(1)
	}

	// ── Apply ACLs to data directory ─────────────────────────────────────────
	if err := os.MkdirAll(cfg.DataDir, 0700); err != nil {
		fatalf("create data dir: %v", err)
	}
	if err := security.LockDirToCurrentUser(cfg.DataDir); err != nil {
		// Non-fatal: log but continue. The directory was created with 0700.
		fmt.Fprintf(os.Stderr, "warning: could not apply ACLs to %s: %v\n", cfg.DataDir, err)
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

	// ── Agent ─────────────────────────────────────────────────────────────────
	ag := agent.NewAgent(sess, apiClient, memMgr, cfg, nil)

	// ── Scheduler ────────────────────────────────────────────────────────────
	sched := scheduler.NewScheduler(database.Conn(), func(ctx context.Context, sessionID, prompt string) error {
		scheduledSess, loadErr := sessionMgr.Load(sessionID)
		if loadErr != nil {
			return fmt.Errorf("scheduler: load session %q: %w", sessionID, loadErr)
		}
		scheduledAgent := agent.NewAgent(scheduledSess, apiClient, memMgr, cfg, nil)
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
	repl := terminal.NewREPL(cfg, sess, ag, sessionMgr, memMgr, sched, noColor)

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

	// Prompt for the API key with echo disabled.
	fmt.Print("Enter your Anthropic API key: ")
	apiKey, err := readPasswordWindows()
	fmt.Println() // newline after masked input
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
		fmt.Fprintf(os.Stderr, "warning: could not apply ACLs to %s: %v\n", cfg.DataDir, err)
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

// readPasswordWindows reads a password from the Windows console with echo
// disabled using ReadConsoleW with ENABLE_ECHO_INPUT cleared.
func readPasswordWindows() (string, error) {
	handle := windows.Handle(os.Stdin.Fd())

	var oldMode uint32
	if err := windows.GetConsoleMode(handle, &oldMode); err != nil {
		// Fallback: read from stdin without masking (e.g., piped input).
		var buf [512]byte
		n, err := os.Stdin.Read(buf[:])
		if err != nil {
			return "", err
		}
		s := string(buf[:n])
		// Trim trailing newline / carriage return.
		for len(s) > 0 && (s[len(s)-1] == '\r' || s[len(s)-1] == '\n') {
			s = s[:len(s)-1]
		}
		return s, nil
	}

	// Disable echo while reading.
	const enableEchoInput = 0x0004
	noEcho := oldMode &^ enableEchoInput
	if err := windows.SetConsoleMode(handle, noEcho); err != nil {
		return "", fmt.Errorf("SetConsoleMode (disable echo): %w", err)
	}
	defer windows.SetConsoleMode(handle, oldMode)

	// Read characters one at a time via ReadConsoleW until CR or LF.
	var result []uint16
	for {
		var (
			buf       [2]uint16
			charsRead uint32
		)
		r, _, e := procReadConsoleW.Call(
			uintptr(handle),
			uintptr(unsafe.Pointer(&buf[0])),
			1,
			uintptr(unsafe.Pointer(&charsRead)),
			0,
		)
		if r == 0 {
			return "", fmt.Errorf("ReadConsoleW: %w", e)
		}
		if charsRead == 0 {
			break
		}
		ch := buf[0]
		if ch == '\r' || ch == '\n' {
			break
		}
		if ch == 0x08 || ch == 0x7F { // Backspace
			if len(result) > 0 {
				result = result[:len(result)-1]
			}
			continue
		}
		result = append(result, ch)
	}

	return windows.UTF16ToString(result), nil
}

var (
	modKernel32      = windows.NewLazySystemDLL("kernel32.dll")
	procReadConsoleW = modKernel32.NewProc("ReadConsoleW")
)

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
