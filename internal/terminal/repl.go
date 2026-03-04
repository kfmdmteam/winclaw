//go:build windows

package terminal

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/windows"
	"golang.org/x/term"

	"winclaw/internal/agent"
	"winclaw/internal/config"
	"winclaw/internal/memory"
	"winclaw/internal/scheduler"
	"winclaw/internal/tools"
)

// ─────────────────────────────────────────────────────────────────────────────
// ANSI colour helpers
// ─────────────────────────────────────────────────────────────────────────────

const (
	ansiReset    = "\x1b[0m"
	ansiBold     = "\x1b[1m"
	ansiDim      = "\x1b[2m"
	ansiFgCyan   = "\x1b[36m"
	ansiFgBCyan  = "\x1b[96m"
	ansiFgGreen  = "\x1b[92m"
	ansiFgYellow = "\x1b[33m"
	ansiFgRed    = "\x1b[91m"
	ansiFgWhite  = "\x1b[97m"
	ansiFgGray   = "\x1b[90m"
	ansiFgPurple = "\x1b[95m"

	// Compound codes used in the REPL.
	colorPrompt  = ansiBold + ansiFgBCyan
	colorInput   = ansiFgWhite
	colorOutput  = ansiFgGreen
	colorCommand = ansiFgYellow
	colorError   = ansiBold + ansiFgRed
	colorSystem  = ansiDim + ansiFgGray
	colorTool    = ansiBold + ansiFgPurple
	colorBanner  = ansiBold + ansiFgBCyan
	colorMeta    = ansiDim + ansiFgCyan
)

// enableVirtualTerminal requests ENABLE_VIRTUAL_TERMINAL_PROCESSING on the
// Windows console so that ANSI escape codes work. Returns true on success.
func enableVirtualTerminal() bool {
	handle := windows.Handle(os.Stdout.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(handle, &mode); err != nil {
		return false
	}
	const enableVTP = 0x0004 // ENABLE_VIRTUAL_TERMINAL_PROCESSING
	if mode&enableVTP != 0 {
		return true // already enabled
	}
	return windows.SetConsoleMode(handle, mode|enableVTP) == nil
}

// ─────────────────────────────────────────────────────────────────────────────
// REPL
// ─────────────────────────────────────────────────────────────────────────────

// REPL is the interactive Read-Eval-Print Loop for WinClaw. It owns a raw-mode
// terminal, draws a coloured prompt, handles line-editing, navigates history,
// dispatches slash commands, and streams agent responses to the terminal.
type REPL struct {
	cfg        *config.Config
	session    *agent.Session
	agentObj   *agent.Agent
	sessionMgr *agent.SessionManager
	memory     *memory.MemoryManager
	scheduler  *scheduler.Scheduler
	toolsMaker func(sess *agent.Session) *tools.Registry
	version    string

	history    []string
	historyIdx int // points one past the last item (insertion point)

	ansiOK  bool // whether the terminal supports ANSI
	noColor bool // user-requested --no-color override

	// mu protects outputLock: prevents prompt re-draw racing with agent output.
	outputMu sync.Mutex

	// cancelAgent cancels the in-flight agent request (if any).
	cancelAgent context.CancelFunc
	agentRunning bool
}

// NewREPL constructs a REPL wired to the given dependencies.
// toolsMaker is called whenever a new agent needs to be constructed for a
// session (initial start, /new, /switch). It receives the target session and
// returns a freshly-bound tools.Registry for that session.
func NewREPL(
	cfg *config.Config,
	sess *agent.Session,
	ag *agent.Agent,
	sessionMgr *agent.SessionManager,
	mem *memory.MemoryManager,
	sched *scheduler.Scheduler,
	noColor bool,
	toolsMaker func(sess *agent.Session) *tools.Registry,
	version string,
) *REPL {
	ansiOK := enableVirtualTerminal()
	return &REPL{
		cfg:        cfg,
		session:    sess,
		agentObj:   ag,
		sessionMgr: sessionMgr,
		memory:     mem,
		scheduler:  sched,
		ansiOK:     ansiOK,
		noColor:    noColor,
		toolsMaker: toolsMaker,
		version:    version,
	}
}

// Run is the main REPL loop. It blocks until the user exits or ctx is
// cancelled. It puts the terminal in raw mode for the duration of the call.
func (r *REPL) Run(ctx context.Context) error {
	fd := int(os.Stdin.Fd())

	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return fmt.Errorf("repl: raw mode: %w", err)
	}
	defer term.Restore(fd, oldState)

	r.printBanner()

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		line, err := r.readLine(ctx)
		if err != nil {
			// io.EOF or ctx cancelled — clean exit.
			r.printSystem("\r\nGoodbye.\r\n")
			return nil
		}

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "/") {
			r.handleCommand(ctx, line)
		} else {
			r.runAgent(ctx, line)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Line editor
// ─────────────────────────────────────────────────────────────────────────────

// lineEditor holds the mutable state of the current input line.
type lineEditor struct {
	buf    []rune // full input buffer
	cursor int    // byte offset within buf (in rune units)
}

func (e *lineEditor) insert(r rune) {
	e.buf = append(e.buf, 0)
	copy(e.buf[e.cursor+1:], e.buf[e.cursor:])
	e.buf[e.cursor] = r
	e.cursor++
}

func (e *lineEditor) deleteBack() {
	if e.cursor == 0 {
		return
	}
	e.buf = append(e.buf[:e.cursor-1], e.buf[e.cursor:]...)
	e.cursor--
}

func (e *lineEditor) deleteForward() {
	if e.cursor >= len(e.buf) {
		return
	}
	e.buf = append(e.buf[:e.cursor], e.buf[e.cursor+1:]...)
}

func (e *lineEditor) moveLeft() {
	if e.cursor > 0 {
		e.cursor--
	}
}

func (e *lineEditor) moveRight() {
	if e.cursor < len(e.buf) {
		e.cursor++
	}
}

func (e *lineEditor) String() string { return string(e.buf) }

// readLine draws the prompt and implements the raw-mode line editor. It
// returns the completed line, or an error when the user presses Ctrl+D or
// the context is cancelled.
func (r *REPL) readLine(ctx context.Context) (string, error) {
	ed := &lineEditor{}
	historySnap := append([]string(nil), r.history...) // snapshot for this line
	histPos := len(historySnap)                         // start after last item
	savedCurrent := ""                                   // preserves the partial line during history nav

	r.drawPrompt(ed)

	// Continuation lines appended here when the user ends a line with \.
	var continuation strings.Builder

	buf := make([]byte, 128)
	for {
		// Read one byte at a time; we decode escape sequences manually.
		n, err := os.Stdin.Read(buf[:1])
		if err != nil || n == 0 {
			return "", fmt.Errorf("stdin: %w", err)
		}

		b := buf[0]

		switch b {
		case 0x04: // Ctrl+D
			if len(ed.buf) == 0 {
				r.print("\r\n")
				return "", fmt.Errorf("EOF")
			}
			// Ctrl+D with content: delete forward character.
			ed.deleteForward()
			r.redrawLine(ed)

		case 0x03: // Ctrl+C
			if r.agentRunning {
				// Cancel the running agent.
				if r.cancelAgent != nil {
					r.cancelAgent()
				}
				r.print("\r\n")
				r.printSystem("[request cancelled]\r\n")
			} else {
				// If idle: exit.
				r.print("\r\n")
				return "", fmt.Errorf("EOF")
			}
			ed = &lineEditor{}
			continuation.Reset()
			r.drawPrompt(ed)

		case 0x0C: // Ctrl+L — clear screen
			r.clearScreen()
			r.drawPrompt(ed)

		case '\r', '\n': // Enter
			line := ed.String()
			r.print("\r\n")

			// Multi-line continuation: trailing backslash.
			if strings.HasSuffix(line, `\`) {
				continuation.WriteString(strings.TrimSuffix(line, `\`))
				continuation.WriteString("\n")
				ed = &lineEditor{}
				r.drawContinuationPrompt()
				continue
			}

			// Assemble final line.
			fullLine := continuation.String() + line
			continuation.Reset()

			// Add non-empty lines to history (deduplicate consecutive duplicates).
			fullLine = strings.TrimSpace(fullLine)
			if fullLine != "" {
				if len(r.history) == 0 || r.history[len(r.history)-1] != fullLine {
					r.history = append(r.history, fullLine)
				}
			}
			return fullLine, nil

		case 0x7F, 0x08: // Backspace / DEL
			ed.deleteBack()
			r.redrawLine(ed)

		case 0x1B: // ESC — start of an escape sequence
			// Read next two bytes.
			seq := make([]byte, 2)
			if _, err := os.Stdin.Read(seq[:1]); err != nil {
				continue
			}
			if seq[0] != '[' {
				continue // not a CSI sequence
			}
			if _, err := os.Stdin.Read(seq[1:]); err != nil {
				continue
			}
			switch seq[1] {
			case 'A': // Up arrow — history back
				if histPos > 0 {
					if histPos == len(historySnap) {
						savedCurrent = ed.String()
					}
					histPos--
					ed = editorFromString(historySnap[histPos])
					r.redrawLine(ed)
				}

			case 'B': // Down arrow — history forward
				if histPos < len(historySnap) {
					histPos++
					var text string
					if histPos == len(historySnap) {
						text = savedCurrent
					} else {
						text = historySnap[histPos]
					}
					ed = editorFromString(text)
					r.redrawLine(ed)
				}

			case 'C': // Right arrow
				ed.moveRight()
				r.redrawLine(ed)

			case 'D': // Left arrow
				ed.moveLeft()
				r.redrawLine(ed)

			case '3': // Delete key sends ESC [ 3 ~
				_ = readByte() // consume the trailing ~
				ed.deleteForward()
				r.redrawLine(ed)
			}

		default:
			// Regular printable character (handle multi-byte UTF-8).
			if b < 0x20 {
				// Other control characters — ignore.
				continue
			}
			// Determine how many bytes this UTF-8 sequence needs.
			needed := utf8RuneLen(b)
			raw := make([]byte, needed)
			raw[0] = b
			for i := 1; i < needed; i++ {
				if _, err := os.Stdin.Read(raw[i : i+1]); err != nil {
					break
				}
			}
			ch, _ := utf8.DecodeRune(raw)
			if ch == utf8.RuneError {
				continue
			}
			ed.insert(ch)
			r.redrawLine(ed)
		}
	}
}

// utf8RuneLen returns the byte length of a UTF-8 sequence whose first byte is b.
func utf8RuneLen(b byte) int {
	switch {
	case b&0x80 == 0:
		return 1
	case b&0xE0 == 0xC0:
		return 2
	case b&0xF0 == 0xE0:
		return 3
	case b&0xF8 == 0xF0:
		return 4
	default:
		return 1
	}
}

// readByte reads and discards one byte from stdin.
func readByte() byte {
	buf := make([]byte, 1)
	os.Stdin.Read(buf)
	return buf[0]
}

func editorFromString(s string) *lineEditor {
	runes := []rune(s)
	return &lineEditor{buf: runes, cursor: len(runes)}
}

// ─────────────────────────────────────────────────────────────────────────────
// Terminal rendering
// ─────────────────────────────────────────────────────────────────────────────

// printBanner renders the startup banner after the terminal enters raw mode.
func (r *REPL) printBanner() {
	sessionName := "default"
	if r.session != nil {
		sessionName = r.session.Name
	}
	ver := r.version
	if ver == "" {
		ver = "v0.1.0"
	}

	box := "  ╭──────────────────────────────────────────╮\r\n"
	box += "  │                                          │\r\n"
	box += fmt.Sprintf("  │   %-38s│\r\n", "WinClaw "+ver)
	box += "  │   Windows-Native Terminal AI Assistant  │\r\n"
	box += "  │                                          │\r\n"
	box += "  ╰──────────────────────────────────────────╯\r\n"

	r.print("\r\n")
	r.print(r.colour(colorBanner, box))
	r.print(r.colour(colorMeta, fmt.Sprintf("  model   · %s\r\n", r.cfg.Model)))
	r.print(r.colour(colorMeta, fmt.Sprintf("  session · %s\r\n", sessionName)))
	r.print("\r\n")
	r.print(r.colour(colorSystem, "  /help for commands · Ctrl+D to exit\r\n"))
	r.print("\r\n")
}

// drawPrompt writes the full prompt and positions the cursor correctly.
func (r *REPL) drawPrompt(ed *lineEditor) {
	prompt := r.promptString()
	line := ed.String()
	// Move cursor to start of line, clear it, print prompt + line.
	r.print("\r" + clearEOL() + prompt + r.colour(colorInput, line))
	// Reposition cursor after possible cursor movement within line.
	if ed.cursor < len(ed.buf) {
		// Move cursor left by the number of runes after the cursor.
		back := len(ed.buf) - ed.cursor
		r.print(fmt.Sprintf("\x1b[%dD", back))
	}
}

// redrawLine redraws the current input line without reprinting the prompt
// prefix — equivalent to drawPrompt but more efficient for frequent edits.
func (r *REPL) redrawLine(ed *lineEditor) {
	r.drawPrompt(ed)
}

func (r *REPL) drawContinuationPrompt() {
	r.print("\r" + clearEOL() + r.colour(colorSystem, "      ↳ "))
}

// promptString returns the formatted prompt text.
func (r *REPL) promptString() string {
	name := "default"
	if r.session != nil {
		name = r.session.Name
	}
	return r.colour(colorPrompt, fmt.Sprintf("[%s] ❯ ", name))
}

func clearEOL() string { return "\x1b[K" }

func (r *REPL) clearScreen() {
	r.print("\x1b[2J\x1b[H")
}

// colour wraps text with ANSI codes when supported, else returns text as-is.
func (r *REPL) colour(code, text string) string {
	if r.noColor || !r.ansiOK {
		return text
	}
	return code + text + ansiReset
}

// print writes raw bytes to stdout. In raw mode we must use \r\n explicitly.
func (r *REPL) print(s string) {
	r.outputMu.Lock()
	defer r.outputMu.Unlock()
	fmt.Fprint(os.Stdout, s)
}

// println writes a line followed by \r\n (necessary in raw mode).
func (r *REPL) println(s string) {
	r.print(s + "\r\n")
}

func (r *REPL) printError(msg string) {
	r.print("\r" + clearEOL() + r.colour(colorError, "error: "+msg) + "\r\n")
}

func (r *REPL) printSystem(msg string) {
	r.print(r.colour(colorSystem, msg))
}

func (r *REPL) printCommand(msg string) {
	r.print("\r" + clearEOL() + r.colour(colorCommand, msg) + "\r\n")
}

// ─────────────────────────────────────────────────────────────────────────────
// Spinner
// ─────────────────────────────────────────────────────────────────────────────

var spinnerFrames = []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")

// startSpinner begins a spinner on a separate line and returns a stop function.
// The caller must invoke stop() to erase the spinner before writing more output.
func (r *REPL) startSpinner(label string) func() {
	done := make(chan struct{})
	var once sync.Once
	stop := func() {
		once.Do(func() {
			close(done)
			// Erase the spinner line.
			r.outputMu.Lock()
			fmt.Fprint(os.Stdout, "\r"+clearEOL())
			r.outputMu.Unlock()
		})
	}

	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		frame := 0
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				r.outputMu.Lock()
				ch := string(spinnerFrames[frame%len(spinnerFrames)])
				fmt.Fprintf(os.Stdout, "\r%s%s %s%s",
					r.colour(colorSystem, ch+" "+label),
					ansiReset, "", "")
				r.outputMu.Unlock()
				frame++
			}
		}
	}()
	return stop
}

// ─────────────────────────────────────────────────────────────────────────────
// Agent invocation
// ─────────────────────────────────────────────────────────────────────────────

// runAgent sends input to the agent, shows the spinner, and streams the result.
func (r *REPL) runAgent(ctx context.Context, input string) {
	agentCtx, cancel := context.WithCancel(ctx)
	r.cancelAgent = cancel
	r.agentRunning = true
	defer func() {
		r.agentRunning = false
		r.cancelAgent = nil
		cancel()
	}()

	// Print a blank line to separate user input from response.
	r.print("\r\n")

	stopSpinner := r.startSpinner("Thinking...")
	var spinnerOnce sync.Once
	ensureSpinnerStopped := func() {
		spinnerOnce.Do(stopSpinner)
	}

	// Wire streaming output: each delta stops the spinner on first call,
	// then writes the chunk directly. Tool-use markers (prefixed \x00TOOL\x00)
	// are rendered in a distinct colour with a visual indicator.
	r.agentObj = agent.NewAgent(
		r.session,
		r.agentObj.Client,
		r.memory,
		r.cfg,
		r.toolsMaker(r.session),
		func(chunk string) {
			ensureSpinnerStopped()
			r.outputMu.Lock()
			if strings.HasPrefix(chunk, "\x00TOOL\x00") {
				name := strings.TrimPrefix(chunk, "\x00TOOL\x00")
				fmt.Fprint(os.Stdout, "\r\n"+r.colour(colorTool, "  ▸ "+name)+"\r\n")
			} else {
				fmt.Fprint(os.Stdout, r.colour(colorOutput, chunk))
			}
			r.outputMu.Unlock()
		},
	)

	_, err := r.agentObj.Run(agentCtx, input)
	ensureSpinnerStopped()

	if err != nil {
		if agentCtx.Err() != nil {
			// Cancelled — already printed message in Ctrl+C handler.
			r.print("\r\n")
			return
		}
		r.print("\r\n")
		r.printError(err.Error())
	} else {
		r.print("\r\n")
	}

	// Persist last-active timestamp (non-fatal on failure).
	_ = r.sessionMgr.UpdateLastActive(r.session.ID)

	// Redraw the prompt so the user can type again.
	r.drawPrompt(&lineEditor{})
}

// ─────────────────────────────────────────────────────────────────────────────
// Command dispatcher
// ─────────────────────────────────────────────────────────────────────────────

func (r *REPL) handleCommand(ctx context.Context, line string) {
	parts := splitArgs(line)
	if len(parts) == 0 {
		return
	}
	cmd := strings.ToLower(parts[0])

	switch cmd {
	case "/help":
		r.cmdHelp()

	case "/new":
		name := "default"
		if len(parts) >= 2 {
			name = parts[1]
		}
		r.cmdNew(ctx, name)

	case "/sessions":
		r.cmdSessions()

	case "/switch":
		if len(parts) < 2 {
			r.printError("/switch requires a session id or name")
			return
		}
		r.cmdSwitch(ctx, parts[1])

	case "/delete":
		if len(parts) < 2 {
			r.printError("/delete requires a session id")
			return
		}
		r.cmdDelete(parts[1])

	case "/reset":
		r.cmdReset()

	case "/memory":
		if len(parts) >= 2 && strings.ToLower(parts[1]) == "edit" {
			r.cmdMemoryEdit()
		} else {
			r.cmdMemoryShow()
		}

	case "/schedule":
		if len(parts) < 2 {
			r.printError("/schedule requires a subcommand: list, add, pause, resume, cancel")
			return
		}
		r.cmdSchedule(parts[1:])

	case "/status":
		r.cmdStatus()

	case "/exit", "/quit":
		// Signal the REPL to exit cleanly via EOF convention.
		r.print("\r\n")
		r.printSystem("Exiting...\r\n")
		os.Exit(0)

	default:
		r.printError(fmt.Sprintf("unknown command %q — type /help for a list", cmd))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Individual command implementations
// ─────────────────────────────────────────────────────────────────────────────

func (r *REPL) cmdHelp() {
	help := []struct{ cmd, desc string }{
		{"/help", "show this help message"},
		{"/new [name]", "create a new session with optional name"},
		{"/sessions", "list all sessions"},
		{"/switch <id-or-name>", "switch to a different session"},
		{"/delete <id>", "soft-delete a session"},
		{"/reset", "clear conversation history (memory file kept)"},
		{"/memory", "show the current CLAUDE.md content"},
		{"/memory edit", "open CLAUDE.md in Notepad for editing"},
		{"/schedule list", "list scheduled tasks for this session"},
		{"/schedule add <name> <cron> <prompt>", "add a scheduled task"},
		{"/schedule pause <id>", "pause a scheduled task"},
		{"/schedule resume <id>", "resume a paused task"},
		{"/schedule cancel <id>", "cancel a scheduled task"},
		{"/status", "show session info and token usage"},
		{"/exit", "exit WinClaw"},
	}

	r.printCommand("\r\nAvailable commands:\r\n")
	for _, h := range help {
		r.print(fmt.Sprintf("  %s%s\r\n",
			r.colour(colorCommand, fmt.Sprintf("%-40s", h.cmd)),
			h.desc))
	}
	r.print("\r\n")
}

func (r *REPL) cmdNew(ctx context.Context, name string) {
	sess, err := r.sessionMgr.Create(name)
	if err != nil {
		r.printError(fmt.Sprintf("create session: %v", err))
		return
	}
	r.session = sess
	r.agentObj = agent.NewAgent(sess, r.agentObj.Client, r.memory, r.cfg, r.toolsMaker(sess), nil)
	r.printSystem(fmt.Sprintf("switched to new session %q (%s)\r\n", sess.Name, sess.ID))
}

func (r *REPL) cmdSessions() {
	sessions, err := r.sessionMgr.List()
	if err != nil {
		r.printError(fmt.Sprintf("list sessions: %v", err))
		return
	}
	if len(sessions) == 0 {
		r.printSystem("no sessions found\r\n")
		return
	}
	r.printCommand("\r\nSessions:\r\n")
	for _, s := range sessions {
		active := ""
		if r.session != nil && s.ID == r.session.ID {
			active = " (active)"
		}
		r.print(fmt.Sprintf("  %s  %s%s\r\n",
			r.colour(colorCommand, s.ID[:8]+"..."),
			s.Name,
			r.colour(colorSystem, active)))
	}
	r.print("\r\n")
}

func (r *REPL) cmdSwitch(ctx context.Context, idOrName string) {
	sessions, err := r.sessionMgr.List()
	if err != nil {
		r.printError(fmt.Sprintf("list sessions: %v", err))
		return
	}

	var target *agent.Session
	for _, s := range sessions {
		if s.ID == idOrName || s.Name == idOrName ||
			strings.HasPrefix(s.ID, idOrName) {
			target = s
			break
		}
	}
	if target == nil {
		r.printError(fmt.Sprintf("session not found: %q", idOrName))
		return
	}

	r.session = target
	r.agentObj = agent.NewAgent(target, r.agentObj.Client, r.memory, r.cfg, r.toolsMaker(target), nil)
	r.printSystem(fmt.Sprintf("switched to session %q (%s)\r\n", target.Name, target.ID))
}

func (r *REPL) cmdDelete(id string) {
	if r.session != nil && (r.session.ID == id || strings.HasPrefix(r.session.ID, id)) {
		r.printError("cannot delete the active session — switch to another first")
		return
	}
	if err := r.sessionMgr.Delete(id); err != nil {
		r.printError(fmt.Sprintf("delete session: %v", err))
		return
	}
	r.printSystem(fmt.Sprintf("session %q deleted\r\n", id))
}

func (r *REPL) cmdReset() {
	r.agentObj.Reset()
	r.printSystem("conversation history cleared (memory file preserved)\r\n")
}

func (r *REPL) cmdMemoryShow() {
	if r.session == nil {
		r.printError("no active session")
		return
	}
	content, err := r.memory.Read(r.session.ID)
	if err != nil {
		r.printError(fmt.Sprintf("read memory: %v", err))
		return
	}
	if strings.TrimSpace(content) == "" {
		r.printSystem("(memory file is empty)\r\n")
		return
	}
	r.print("\r\n" + r.colour(colorOutput, content) + "\r\n")
}

func (r *REPL) cmdMemoryEdit() {
	if r.session == nil {
		r.printError("no active session")
		return
	}
	// Ensure the session directory exists before opening the editor.
	if err := r.memory.InitSession(r.session.ID); err != nil {
		r.printError(fmt.Sprintf("prepare memory dir: %v", err))
		return
	}
	memPath := r.memory.MemoryPath(r.session.ID)

	// Restore normal console mode while Notepad runs.
	fd := int(os.Stdin.Fd())
	old, _ := term.GetState(fd)

	_ = term.Restore(fd, old)
	cmd := exec.Command("notepad.exe", memPath)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		// notepad returns non-zero if the user closes without saving — ignore.
		_ = err
	}

	// Re-enter raw mode.
	_, _ = term.MakeRaw(fd)
	r.printSystem("memory file updated\r\n")
}

func (r *REPL) cmdSchedule(args []string) {
	if len(args) == 0 {
		r.printError("usage: /schedule list|add|pause|resume|cancel")
		return
	}
	switch strings.ToLower(args[0]) {
	case "list":
		r.scheduleList()
	case "add":
		// /schedule add <name> <cron> <prompt...>
		if len(args) < 4 {
			r.printError("usage: /schedule add <name> <cron> <prompt>")
			return
		}
		name := args[1]
		cron := args[2]
		prompt := strings.Join(args[3:], " ")
		r.scheduleAdd(name, cron, prompt)
	case "pause":
		if len(args) < 2 {
			r.printError("/schedule pause requires an id")
			return
		}
		r.schedulePause(args[1])
	case "resume":
		if len(args) < 2 {
			r.printError("/schedule resume requires an id")
			return
		}
		r.scheduleResume(args[1])
	case "cancel":
		if len(args) < 2 {
			r.printError("/schedule cancel requires an id")
			return
		}
		r.scheduleCancel(args[1])
	default:
		r.printError(fmt.Sprintf("unknown schedule subcommand %q", args[0]))
	}
}

func (r *REPL) scheduleList() {
	if r.session == nil {
		r.printError("no active session")
		return
	}
	tasks, err := r.scheduler.List(r.session.ID)
	if err != nil {
		r.printError(fmt.Sprintf("list tasks: %v", err))
		return
	}
	if len(tasks) == 0 {
		r.printSystem("no scheduled tasks for this session\r\n")
		return
	}
	r.printCommand("\r\nScheduled tasks:\r\n")
	for _, t := range tasks {
		r.print(fmt.Sprintf("  %s  %-20s  %-15s  status:%-10s  next:%s\r\n",
			r.colour(colorCommand, t.ID[:8]+"..."),
			t.Name,
			t.Schedule,
			t.Status,
			t.NextRun.Local().Format("2006-01-02 15:04")))
	}
	r.print("\r\n")
}

func (r *REPL) scheduleAdd(name, cron, prompt string) {
	if r.session == nil {
		r.printError("no active session")
		return
	}
	id, err := r.scheduler.Schedule(r.session.ID, name, cron, prompt)
	if err != nil {
		r.printError(fmt.Sprintf("add task: %v", err))
		return
	}
	r.printSystem(fmt.Sprintf("task %q added (id: %s)\r\n", name, id))
}

func (r *REPL) schedulePause(id string) {
	if err := r.scheduler.Pause(id); err != nil {
		r.printError(fmt.Sprintf("pause task: %v", err))
		return
	}
	r.printSystem(fmt.Sprintf("task %q paused\r\n", id))
}

func (r *REPL) scheduleResume(id string) {
	if err := r.scheduler.Resume(id); err != nil {
		r.printError(fmt.Sprintf("resume task: %v", err))
		return
	}
	r.printSystem(fmt.Sprintf("task %q resumed\r\n", id))
}

func (r *REPL) scheduleCancel(id string) {
	if err := r.scheduler.Cancel(id); err != nil {
		r.printError(fmt.Sprintf("cancel task: %v", err))
		return
	}
	r.printSystem(fmt.Sprintf("task %q cancelled\r\n", id))
}

func (r *REPL) cmdStatus() {
	if r.session == nil {
		r.printSystem("no active session\r\n")
		return
	}

	// Count in-memory message turns.
	turns := len(r.session.Messages) / 2

	// Approximate token usage by character count (rough heuristic).
	var totalChars int
	for _, msg := range r.session.Messages {
		switch c := msg.Content.(type) {
		case string:
			totalChars += len(c)
		}
	}

	// Memory file size.
	memContent, _ := r.memory.Read(r.session.ID)
	memSize := len(memContent)

	r.printCommand("\r\nSession status:\r\n")
	r.print(fmt.Sprintf("  ID          : %s\r\n", r.session.ID))
	r.print(fmt.Sprintf("  Name        : %s\r\n", r.session.Name))
	r.print(fmt.Sprintf("  Created     : %s\r\n", r.session.CreatedAt.Local().Format(time.RFC3339)))
	r.print(fmt.Sprintf("  Last active : %s\r\n", r.session.LastActive.Local().Format(time.RFC3339)))
	r.print(fmt.Sprintf("  Turns       : %d\r\n", turns))
	r.print(fmt.Sprintf("  Approx chars: %d\r\n", totalChars))
	r.print(fmt.Sprintf("  Memory size : %d bytes\r\n", memSize))
	r.print(fmt.Sprintf("  Model       : %s\r\n", r.cfg.Model))
	r.print("\r\n")
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// splitArgs splits a command line respecting double-quoted tokens.
func splitArgs(line string) []string {
	var args []string
	var cur strings.Builder
	inQuote := false
	for _, r := range line {
		switch {
		case r == '"':
			inQuote = !inQuote
		case r == ' ' && !inQuote:
			if cur.Len() > 0 {
				args = append(args, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		args = append(args, cur.String())
	}
	return args
}
