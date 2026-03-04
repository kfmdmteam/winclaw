# Changelog

All notable changes to WinClaw are documented here.

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
