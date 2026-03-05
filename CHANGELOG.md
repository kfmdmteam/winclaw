# Changelog

All notable changes to WinClaw are documented here.

---

## [0.2.0] — 2026-03-05

### Added

**Windows Service mode**
- `--install-service` / `--uninstall-service` flags register and remove WinClaw
  as a Windows Service running under `NT AUTHORITY\LocalService`.
- `--start-service` / `--stop-service` / `--service-status` flags control the
  service via the Service Control Manager without requiring elevation after
  initial install.
- Machine-DPAPI (`CryptProtectData` with `CRYPTPROTECT_LOCAL_MACHINE`) encrypts
  the API key into `service-key.enc` so the service can read it without access
  to the installing user's Credential Manager session.
- `internal/service/` package implements the `svc.Handler` interface:
  startup, scheduler launch, named pipe server, SCM command loop, and clean
  shutdown with in-flight-task draining.

**Named Pipe IPC (`--send`)**
- `--send "<prompt>"` connects to the running service over `\\.\pipe\WinClaw`
  and streams the response to stdout — usable from scripts, scheduled tasks,
  and other terminals.
- Pipe DACL grants access only to `SYSTEM`, `LocalService`, and the SID of the
  user who ran `--install-service`; anonymous connections are rejected.
- `internal/ipc/pipe.go`: length-prefixed JSON frame protocol (server + client).

**Extended Thinking**
- `/think` toggles extended thinking on the current session agent.
- When enabled, each API request carries `"thinking": {"type":"enabled",
  "budget_tokens": 10000}` and the `anthropic-beta: interleaved-thinking-…`
  header.
- `/status` now shows the current thinking state.

**Image Attachment**
- `/attach <path>` attaches a PNG/JPEG/GIF/WEBP file; it is included as a
  base64 image block with your next message.
- `--attach <path>` attaches an image at startup so it can be used with
  `--send` or a REPL session.

**Soul File**
- `%APPDATA%\WinClaw\SOUL.md` is a persistent identity/values file injected
  into every system prompt ahead of session memory.
- Default soul is written on first run; describes WinClaw's persona, tools,
  operating rules, and Windows-native knowledge.
- `/soul` displays the current soul file; `/soul edit` opens it in Notepad.
- `update_soul` tool lets the model rewrite its own soul file when
  self-understanding evolves.

**Global Cross-Session Memory**
- `%APPDATA%\WinClaw\GLOBAL.md` persists across all sessions.
- `/global` displays the global memory file.
- `update_global_memory` tool lets the model append facts that should survive
  across session boundaries (user preferences, long-lived project context).

**Windows-Native Tools**
- `screenshot` — captures the primary monitor as PNG via GDI+; the image is
  returned as a base64 block so the model can visually analyse the screen.
- `process_list` — lists running processes by CPU/memory usage via `Get-Process`.
- `kill_process` — terminates a process by PID or name.
- `toast_notify` — sends a Windows desktop balloon notification via
  `System.Windows.Forms.NotifyIcon`.
- `run_elevated` — runs a PowerShell command via `Start-Process -Verb RunAs`
  (triggers UAC); output is captured via a temp file.
- `registry_read` / `registry_write` — reads/writes Windows registry values
  via `Get-ItemProperty` / `Set-ItemProperty`.

**Plugin System**
- `%APPDATA%\WinClaw\plugins\*.ps1` files with a `# WinClaw-Plugin` header
  block are loaded at startup and exposed as agent tools.
- Plugin parameters are declared in a `# Parameters:` header line as a JSON
  Schema object.
- Plugins are called with `-InputJson '<json>'`; output is returned verbatim.

**Delegate Tool**
- `delegate` tool spawns a fresh sub-agent with a clean context to handle
  isolated sub-tasks, then returns the result to the parent agent.
- Sub-agents inherit all tools (except `delegate` itself to prevent recursion
  inadvertently) and share global memory.

**Auto-Consolidation**
- When a session memory file exceeds 8 KB, the agent automatically summarises
  it in the background using a separate non-streaming API call (2048-token
  budget). The consolidated result replaces the raw file.

**REPL additions**
- `/global` — display the global cross-session memory file.
- `/soul` / `/soul edit` — display or edit the soul file.
- `/attach <path>` — attach an image to the next message.
- `/think` — toggle extended thinking.
- `/status` now shows actual API token usage (accumulated across all turns in
  the session) and the current thinking mode.

**Auto-context in system prompt**
- Working directory and git branch are injected automatically into every system
  prompt under `## Environment`, giving the model situational awareness without
  requiring the user to state the context explicitly.

### Fixed

- `version` changed from `const` to `var` so `-ldflags "-X main.version=…"` in
  `build.bat` correctly injects the version at link time.
- Single-quote injection in Windows-native tool PowerShell scripts: all
  user-supplied strings embedded in PS single-quoted literals are now sanitised
  with `psEscape()` (doubles `'` to `''`). Affected tools: `process_list`,
  `kill_process`, `toast_notify`, `registry_read`, `registry_write`.
- `registry_write` now validates the `type` argument against an allowlist
  (`String`, `DWord`, `QWord`, `Binary`, `ExpandString`, `MultiString`) before
  inserting it unquoted into the PowerShell command.

---

## [0.1.1] — 2026-03-04

Security audit fixes. No behaviour changes for normal usage.

### Fixed

- **Removed dead code: `internal/ipc/`** — The Named Pipe IPC server and
  client were implemented but never started or called anywhere in the codebase.
  WinClaw is a single-process application; there are no other processes to
  communicate with. The code was removed rather than left as dormant
  infrastructure that implied security properties it did not enforce.

- **Removed dead code: `internal/security/jobobj.go`** — Windows Job Objects
  were implemented but `NewJobObject()` was never called. No subprocess is
  spawned in the current architecture, so there was nothing to assign to a job.
  Removed to match reality.

- **API key zeroed on exit** — `security.ReadSecret` now returns `[]byte`
  instead of `string`. `api.Client` stores the key as `[]byte` and exposes a
  `Close()` method that calls `clear()` on the slice. `main.go` defers
  `apiClient.Close()` so the key is zeroed from heap memory on shutdown,
  reducing the window in which a memory dump would expose it.

- **ACL failure is now fatal** — `LockDirToCurrentUser` failure previously
  printed a warning and allowed startup to continue with an unprotected data
  directory. It now calls `fatalf`, aborting startup with a clear message
  directing the user to check `SeSecurityPrivilege`.

- **Input length limits** — Added validation at the entry points for
  user-controlled strings that are stored in SQLite:
  - Session names: 1–128 characters
  - Schedule task names: 1–128 characters
  - Cron/interval expressions: 1–64 characters
  - Task prompts: 1–10,000 characters

- **README: removed false certificate-pinning claim** — The README stated
  "Certificate pinning for Anthropic API" under security controls. The code
  only enforces TLS 1.2 minimum via `tls.Config.MinVersion`; no pinning was
  implemented. The entry has been corrected.

- **README: removed Job Objects and Named Pipes from security table** — Both
  were listed as active security controls but were dead code. The table now
  reflects only what the running binary actually enforces.

---

Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Versioning follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

---

## [0.1.0] — 2026-03-04

Initial release of WinClaw — a Windows-native, terminal-only AI assistant built
around native Windows security primitives rather than Docker or Unix tooling.

### Added

**Core runtime**
- Single Go binary targeting Windows x64; no Node.js, no Docker, no runtime dependencies
- `//go:build windows` tags on all Windows-specific packages enforce a clean
  compile-time boundary between platform code and portable logic
- Graceful shutdown on `SIGINT` / `os.Interrupt` with in-flight task draining
- `--version`, `--setup`, `--session`, `--model`, `--no-color`, `--log-level` flags

**Security**
- Windows Credential Manager integration via `advapi32.dll` (`CredReadW`,
  `CredWriteW`, `CredDeleteW`) — API keys are stored as DPAPI-protected blobs
  and never written to disk
- Windows ACL helper that applies a `PROTECTED_DACL` to the data directory,
  granting `GENERIC_ALL` only to the current user SID and `NT AUTHORITY\SYSTEM`
- Windows Job Objects for subprocess isolation — memory cap, CPU rate cap,
  and `KillOnJobClose` so children cannot outlive the parent
- Named Pipe IPC server/client with a per-user DACL built from the current
  process token; no anonymous connections accepted
- First-time setup wizard with echo-disabled password input via `ReadConsoleW`
- `audit_log` SQLite table records every security-relevant event with timestamp,
  action, subject, detail, and success flag
- Windows Event Log integration (source: `WinClaw`, IDs 1–4) with transparent
  fallback to stderr when the source is not yet registered

**Persistence**
- SQLite database (`modernc.org/sqlite`, pure Go, no CGO) in WAL mode with
  foreign-key enforcement, `synchronous=NORMAL`, and a 32 MiB page cache
- Four-table schema: `sessions`, `messages`, `scheduled_tasks`, `audit_log`
- Soft-delete on sessions (`deleted = 0/1`) preserves the audit log
- Idempotent migration runner — safe to re-execute on an existing database

**Agent and API**
- Anthropic Messages API client with TLS 1.2 minimum, 120s timeout,
  and exponential back-off retry (3 attempts, 1 s / 2 s / 4 s) on 429/500/503
- Server-sent events (SSE) streaming parser — tokens are written to the
  terminal as they arrive rather than waiting for the full response
- Token-bucket rate limiter (default 50 req/min, configurable)
- Running `TokensUsed` counter on each `Agent` instance, populated from the
  `usage` field in every API response

**Token efficiency**
- Sliding history window — only the most recent `history_window` turns
  (default 20, configurable) are included in each API request; the full history
  is retained locally for audit but excluded from the wire payload
- Compact system prompt — one-line identity statement and current date only;
  the session UUID is omitted to save tokens
- Input trimming — leading and trailing whitespace stripped before appending
  to history and before sending to the API

**Session and memory**
- UUID-keyed sessions with `created_at` / `last_active` timestamps
- Per-session `MEMORY.md` file under `%APPDATA%\WinClaw\sessions\<id>\`
- Memory files written atomically (`.tmp` + rename) with `0600` permissions
- `MemoryManager` is safe for concurrent use from multiple goroutines

**Scheduler**
- 30-second poll loop for due tasks
- Three schedule types: 5-field cron, `@every <duration>`, `@once`
- `@every` supports Go durations plus `Nd` (days) and `Nw` (weeks)
- Cron parser handles `*`, `*/n`, `a-b`, `a-b/n`, and comma lists on all
  five fields with a 2-year search window safety limit
- `pause`, `resume`, `cancel` lifecycle operations
- `@once` tasks automatically transition to `cancelled` after execution
- Invalid schedules are paused automatically to prevent retry storms

**Terminal REPL**
- Raw-mode line editor via `golang.org/x/term` — no external readline library
- Arrow key history navigation, left/right cursor movement, backspace/delete
- Multi-line input with trailing `\` continuation
- Animated spinner (`⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏`) while the agent is thinking,
  cleared cleanly before the response is printed
- ANSI colour output (bold cyan prompt, green agent text, yellow commands,
  red errors) with automatic `ENABLE_VIRTUAL_TERMINAL_PROCESSING` via
  `SetConsoleMode`; graceful fallback to plain text
- `Ctrl+C` cancels the current in-progress request; a second `Ctrl+C` (or
  `Ctrl+D`) exits cleanly
- Slash commands: `/help`, `/new`, `/sessions`, `/switch`, `/delete`,
  `/reset`, `/memory`, `/memory edit`, `/schedule list/add/pause/resume/cancel`,
  `/status`, `/exit`

**Configuration**
- `%APPDATA%\WinClaw\config.json` with atomic save
- Fields: `model`, `max_tokens`, `history_window`, `max_concurrent_agents`,
  `agent_timeout_seconds`, `stream_responses`, `log_level`
- Unknown JSON fields are silently ignored; missing fields fall back to
  compile-time defaults so config files remain forward-compatible

**Build**
- `build.bat` runs `go mod tidy`, `go vet ./...`, and produces `winclaw.exe`
- `build.bat release` strips debug symbols (`-s -w`) for a smaller binary
- All Windows-specific files carry `//go:build windows` so the module can be
  statically analysed on other platforms

---

[0.1.0]: https://github.com/your-username/winclaw/releases/tag/v0.1.0
