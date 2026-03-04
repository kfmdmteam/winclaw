# WinClaw

**A Windows-native, terminal-only AI assistant built for security and efficiency.**

WinClaw brings Claude AI to the Windows command line with zero runtime dependencies, no Docker, and no exposed ports. Every secret lives in Windows Credential Manager. Every process runs inside a Windows Job Object. Every session is isolated with a per-user ACL.

---

## Why WinClaw

Most AI assistant tools are built for macOS or Linux and ported to Windows as an afterthought. They rely on Docker for isolation, store credentials in `.env` files, and expose local web servers or WebSocket gateways. On Windows, this pattern introduces unnecessary attack surface and operational overhead.

WinClaw takes the opposite approach:

- **Single binary.** No Node.js, no Docker, no npm. One `.exe`, nothing else.
- **Secrets never touch disk.** The API key is stored in Windows Credential Manager via DPAPI and read directly into memory at startup.
- **No listening ports.** The only network connection WinClaw makes is outbound HTTPS to the Anthropic API.
- **Terminal only.** No web UI, no WebSocket gateway. Access is gated entirely by who can open a terminal on the machine.

---

## Security Architecture

```
┌─────────────────────────────────────────────────────────────┐
│  Windows Credential Manager (DPAPI)                         │
│  └── WinClaw/AnthropicAPIKey  ← never written to disk       │
└──────────────────────┬──────────────────────────────────────┘
                       │ read at startup only
┌──────────────────────▼──────────────────────────────────────┐
│  winclaw.exe  (single process, no root required)            │
│                                                             │
│  ┌─────────────┐  ┌──────────────┐  ┌────────────────────┐ │
│  │  Terminal   │  │  Scheduler   │  │  Named Pipe IPC    │ │
│  │  REPL       │  │  (cron/      │  │  (current-user     │ │
│  │  (raw mode) │  │   interval)  │  │   DACL only)       │ │
│  └──────┬──────┘  └──────┬───────┘  └────────────────────┘ │
│         │                │                                  │
│  ┌──────▼────────────────▼──────────────────────────────┐  │
│  │  Agent  (windowed history · compact system prompt)   │  │
│  └──────────────────────┬───────────────────────────────┘  │
│                         │ TLS 1.2+ outbound only           │
└─────────────────────────┼───────────────────────────────────┘
                          ▼
               api.anthropic.com/v1/messages
```

### Security controls at a glance

| Layer | Control |
|---|---|
| **Secrets** | Windows Credential Manager (DPAPI) — no `.env`, no plaintext |
| **Data directory** | ACL: deny Everyone, grant current user + SYSTEM (`GENERIC_ALL`) |
| **Subprocesses** | Windows Job Objects — memory cap, CPU cap, `KillOnJobClose` |
| **IPC** | Named Pipes with per-user DACL; no Unix sockets, no TCP |
| **Audit trail** | Windows Event Log (Application log, Source: WinClaw) + `audit_log` table |
| **Network** | Outbound HTTPS only, TLS 1.2 minimum, no listening ports |
| **Binary** | Built with `-s -w` (no debug symbols, stripped), Windows-only build tag |
| **Database** | SQLite WAL, foreign keys enforced, `0600` file permissions |
| **Memory files** | Written atomically (`tmp` + rename), `0600` permissions |

---

## Token Efficiency

Keeping API costs low is a first-class concern. WinClaw applies three layers of optimisation on every request:

**1. Sliding history window**
Only the most recent `history_window` turns (default: 20) are sent to the API. The full conversation is retained locally for audit, but older messages are excluded from the wire payload. On a long session this can reduce input tokens by 60–80%.

**2. Compact system prompt**
The system prompt is intentionally minimal — a one-line identity statement and the current date. The session ID and verbose preamble found in most agent frameworks are omitted. The only way to grow the system prompt is to write to `MEMORY.md`, which you control.

**3. Input trimming**
All whitespace is stripped from user input before it is appended to the history. Accidental leading/trailing newlines and spaces are never billed.

You can tune the window at any time by editing `config.json`:

```json
{
  "history_window": 10
}
```

Halving the window roughly halves input token cost on mature sessions.

---

## Requirements

- Windows 10 / Windows 11 (x64)
- Go 1.21+ ([go.dev/dl](https://go.dev/dl/))
- An Anthropic API key ([console.anthropic.com](https://console.anthropic.com))

---

## Installation

```bat
git clone https://github.com/your-username/winclaw.git
cd winclaw
build.bat release
```

The build script runs `go mod tidy`, `go vet`, and produces `winclaw.exe` in the project root.

---

## First-time setup

```bat
winclaw.exe --setup
```

The setup wizard:
1. Prompts for your Anthropic API key with echo disabled
2. Stores it in Windows Credential Manager under `WinClaw/AnthropicAPIKey`
3. Creates `%APPDATA%\WinClaw\` with a per-user ACL
4. Writes the default `config.json`

No `.env` file is created. The key never appears in the file system.

---

## Usage

```bat
winclaw.exe
```

```
WinClaw v0.1.0 — Windows-Native Terminal AI Assistant
winclaw[default]>
```

### Terminal commands

| Command | Description |
|---|---|
| `/help` | List all commands |
| `/new [name]` | Create a new session |
| `/sessions` | List all sessions |
| `/switch <id>` | Switch to a session |
| `/delete <id>` | Delete a session |
| `/reset` | Clear conversation history (keeps memory) |
| `/memory` | Show the current session's MEMORY.md |
| `/memory edit` | Open MEMORY.md in Notepad |
| `/schedule list` | List scheduled tasks |
| `/schedule add <name> <cron> <prompt>` | Add a scheduled task |
| `/schedule pause <id>` | Pause a task |
| `/schedule resume <id>` | Resume a task |
| `/schedule cancel <id>` | Cancel a task |
| `/status` | Session info and token usage |
| `/exit` | Quit |

### Key bindings

| Key | Action |
|---|---|
| `Up` / `Down` | Navigate history |
| `Left` / `Right` | Move cursor |
| `Ctrl+C` | Cancel in-progress request (or exit if idle) |
| `Ctrl+D` | Exit cleanly |
| `Ctrl+L` | Clear screen |
| `\` (line end) | Continue input on next line |

### Schedule syntax

```bat
# Cron (5-field: min hour dom mon dow)
/schedule add daily-summary "0 9 * * *" "Summarise yesterday's work"

# Interval
/schedule add check-in "@every 2h" "What should I focus on?"

# One-time
/schedule add reminder "@once" "Remind me to review the deploy"
```

---

## Configuration

`%APPDATA%\WinClaw\config.json`

```json
{
  "model": "claude-sonnet-4-6",
  "max_tokens": 8192,
  "history_window": 20,
  "max_concurrent_agents": 4,
  "agent_timeout_seconds": 300,
  "stream_responses": true,
  "log_level": "info"
}
```

| Field | Default | Description |
|---|---|---|
| `model` | `claude-sonnet-4-6` | Anthropic model |
| `max_tokens` | `8192` | Max tokens per response |
| `history_window` | `20` | Turns kept in API context |
| `max_concurrent_agents` | `4` | Parallel agent limit |
| `agent_timeout_seconds` | `300` | Per-agent deadline |
| `stream_responses` | `true` | Stream tokens to terminal |
| `log_level` | `info` | `debug` / `info` / `warning` / `error` |

---

## Data layout

```
%APPDATA%\WinClaw\
├── config.json                  ← settings (no secrets)
├── winclaw.db                   ← SQLite (sessions, messages, tasks, audit log)
└── sessions\
    └── <session-id>\
        └── MEMORY.md            ← per-session context injected into system prompt
```

All directories are created with `0700` and have an explicit ACL applied at startup. The database uses WAL journal mode and `synchronous=NORMAL`.

---

## Windows Event Log

WinClaw writes to the Application event log under the source `WinClaw`. To view:

```
eventvwr.msc → Windows Logs → Application → filter by Source: WinClaw
```

Security events (startup, shutdown, key operations) are written at Event ID 4 with the format:

```
SECURITY | action=startup subject=winclaw detail=version=v0.1.0
```

If Event Log is unavailable (e.g. insufficient permissions on first run before registration), all output falls back to stderr.

---

## Build flags

```bat
:: Debug build (includes symbols, faster iteration)
build.bat

:: Release build (stripped, smaller binary)
build.bat release
```

Manual build:
```bat
go build -tags windows -ldflags="-s -w" -o winclaw.exe ./cmd/winclaw
```

---

## Project layout

```
winclaw/
├── cmd/winclaw/main.go          ← entry point, setup wizard, signal handling
├── internal/
│   ├── agent/
│   │   ├── agent.go             ← run loop, windowed history, token tracking
│   │   └── session.go           ← session CRUD, message audit log
│   ├── api/
│   │   ├── client.go            ← HTTP client, SSE streaming, retry/backoff
│   │   ├── models.go            ← request/response structs
│   │   └── ratelimit.go         ← token-bucket rate limiter
│   ├── config/
│   │   └── config.go            ← config load/save, validation
│   ├── db/
│   │   ├── db.go                ← SQLite open, pragmas, migrations
│   │   └── schema.go            ← DDL constants
│   ├── ipc/
│   │   └── pipes.go             ← named pipe IPC with per-user DACL
│   ├── logging/
│   │   └── eventlog.go          ← Windows Event Log with stderr fallback
│   ├── memory/
│   │   └── memory.go            ← per-session MEMORY.md read/write
│   ├── scheduler/
│   │   └── scheduler.go         ← cron/@every/@once task scheduler
│   ├── security/
│   │   ├── acl.go               ← Windows ACL helpers
│   │   ├── credman.go           ← Windows Credential Manager (DPAPI)
│   │   └── jobobj.go            ← Windows Job Objects
│   └── terminal/
│       └── repl.go              ← raw-mode REPL, line editor, spinner
├── build.bat                    ← build script
├── go.mod
└── CHANGELOG.md
```

---

## Comparison

| | WinClaw | NanoClaw | OpenClaw |
|---|---|---|---|
| **Language** | Go | TypeScript | TypeScript |
| **Runtime** | None (single binary) | Node.js | Node.js |
| **Isolation** | Windows Job Objects | Docker / Apple Container | Docker |
| **Secret storage** | Windows Credential Manager | `.env` files | `.env` files |
| **IPC** | Named Pipes (per-user ACL) | Unix domain sockets | WebSocket gateway |
| **Interface** | Terminal only | Multi-channel messaging | Multi-channel + web UI |
| **Audit log** | Windows Event Log + SQLite | Console | Console |
| **Startup time** | ~50ms | ~2–3s | ~3–5s |
| **Memory use** | ~8 MB | ~80–120 MB | ~150–200 MB |
| **Platforms** | Windows only | macOS / Linux | macOS / Linux / Windows |

---

## License

MIT
