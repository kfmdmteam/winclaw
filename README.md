# WinClaw

**A Windows-native, terminal-only AI assistant built for security and efficiency.**

WinClaw brings Claude AI to the Windows command line with zero runtime dependencies, no Docker, and no exposed ports. Every secret lives in Windows Credential Manager. Every session directory is locked with a per-user ACL. The only network traffic is outbound HTTPS to the Anthropic API.

---

## Why WinClaw

Most AI assistant tools are built for macOS or Linux and ported to Windows as an afterthought. They rely on Docker for isolation, store credentials in `.env` files, and expose local web servers or WebSocket gateways. On Windows, this pattern introduces unnecessary attack surface and operational overhead.

WinClaw takes the opposite approach:

- **Single binary.** No Node.js, no Docker, no npm. One `.exe`, nothing else.
- **Secrets never touch disk.** The API key is stored in Windows Credential Manager via DPAPI and read directly into memory at startup, then zeroed on exit.
- **No listening ports.** The only network connection WinClaw makes is outbound HTTPS to the Anthropic API.
- **Terminal only.** No web UI, no WebSocket gateway. Access is gated entirely by who can open a terminal on the machine.

---

## Security Architecture

```
┌─────────────────────────────────────────────────────────────┐
│  Windows Credential Manager (DPAPI)                         │
│  └── WinClaw/AnthropicAPIKey  ← never written to disk       │
└──────────────────────┬──────────────────────────────────────┘
                       │ read at startup, zeroed on exit
┌──────────────────────▼──────────────────────────────────────┐
│  winclaw.exe  (single process, no root required)            │
│                                                             │
│  ┌─────────────────────┐      ┌────────────────────────┐   │
│  │  Terminal REPL      │      │  Scheduler             │   │
│  │  (raw mode, no GUI) │      │  (cron / interval)     │   │
│  └──────────┬──────────┘      └───────────┬────────────┘   │
│             └──────────────┬──────────────┘                 │
│                    ┌───────▼───────────────────────────┐    │
│                    │  Agent  (windowed history ·        │    │
│                    │          compact system prompt)    │    │
│                    └───────────────┬───────────────────┘    │
│                                    │ TLS 1.2+ outbound only  │
└────────────────────────────────────┼────────────────────────┘
                                     ▼
                          api.anthropic.com/v1/messages
```

### Security controls

| Layer | Control |
|---|---|
| **Secrets** | Windows Credential Manager (DPAPI) — no `.env`, no plaintext |
| **Data directory** | ACL: deny Everyone, grant current user + SYSTEM (`GENERIC_ALL`) — failure is fatal |
| **Audit trail** | Windows Event Log (Application log, Source: WinClaw) + `audit_log` SQLite table |
| **Network** | Outbound HTTPS only, TLS 1.2 minimum, no listening ports |
| **Binary** | Built with `-s -w` (stripped), `//go:build windows` enforced |
| **Database** | SQLite WAL, foreign keys enforced, file created with `0600` permissions |
| **Memory files** | Written atomically (`tmp` + rename), `0600` permissions |
| **API key in memory** | Stored as `[]byte`, zeroed via `client.Close()` on exit |
| **Input validation** | Session names ≤128 chars, prompts ≤10,000 chars, schedules ≤64 chars |

---

## Token Efficiency

Keeping API costs low is a first-class concern. WinClaw applies three layers of optimisation on every request:

**1. Sliding history window**
Only the most recent `history_window` turns (default: 20) are sent to the API. The full conversation is retained locally for audit, but older messages are excluded from the wire payload. On a long session this can reduce input tokens by 60–80%.

**2. Compact system prompt**
The system prompt is intentionally minimal — a one-line identity statement and the current date. The only way to grow it is to write to `MEMORY.md`, which you control.

**3. Input trimming**
All whitespace is stripped from user input before it is appended to the history. Accidental leading/trailing newlines are never billed.

Tune the window at any time:
```json
{ "history_window": 10 }
```
Halving the window roughly halves input token cost on mature sessions.

---

## Full Setup Guide

### Step 1 — Install Git

If you don't have Git installed:

1. Download from [git-scm.com/download/win](https://git-scm.com/download/win)
2. Run the installer, accepting defaults
3. Open a new terminal and verify:
```bat
git --version
```

---

### Step 2 — Install Go 1.21 or later

1. Open [go.dev/dl](https://go.dev/dl/) in your browser
2. Download the Windows `.msi` installer for the latest stable release (e.g. `go1.22.x.windows-amd64.msi`)
3. Run the installer — it adds Go to your PATH automatically
4. **Close and reopen your terminal**, then verify:
```bat
go version
```
You should see something like `go version go1.22.3 windows/amd64`.

> Go must be 1.21 or later. Earlier versions lack the `clear()` builtin used by the security package.

---

### Step 3 — Get an Anthropic API key

1. Go to [console.anthropic.com](https://console.anthropic.com) and sign in
2. Navigate to **API Keys** in the left sidebar
3. Click **Create Key**, give it a name (e.g. `winclaw`), and copy the key
4. Keep the key somewhere safe — you will paste it into the setup wizard and it will never be stored in a file

---

### Step 4 — Clone and build

Open **Command Prompt** or **PowerShell** (not Git Bash — the terminal REPL requires a real Windows console):

```bat
git clone https://github.com/kfmdmteam/winclaw.git
cd winclaw
```

**Command Prompt:**
```bat
build.bat release
```

**PowerShell** — prefix with `.\` (PowerShell does not run scripts from the current directory without it):
```powershell
.\build.bat release
```

The build script:
- Runs `go mod tidy` to download all dependencies (~4 packages, no large frameworks)
- Runs `go vet ./...` to catch any issues
- Produces `winclaw.exe` in the project root

Expected output:
```
WinClaw Build Script

Go: go1.22.3 windows/amd64

Fetching dependencies...
Dependencies OK.

Running go vet...
Vet OK.

Building release binary...

Build successful: winclaw.exe

First-time setup:
  winclaw.exe --setup

Start WinClaw:
  winclaw.exe
```

---

### Step 5 — Run first-time setup

```bat
winclaw.exe --setup
```

The wizard will:

1. Prompt for your Anthropic API key — **input is masked, nothing is echoed to the terminal**
2. Store the key in **Windows Credential Manager** under `WinClaw/AnthropicAPIKey` (DPAPI-encrypted, tied to your Windows user account)
3. Create `%APPDATA%\WinClaw\` and apply a per-user ACL — if this fails (e.g. insufficient privilege), setup aborts rather than continuing with an unlocked directory
4. Write `%APPDATA%\WinClaw\config.json` with default settings

No `.env` file is created. The key never appears in the file system.

> **To verify the key was stored:** open `credential manager` in the Windows Start menu → Windows Credentials → look for `WinClaw/AnthropicAPIKey`.

---

### Step 6 — Start WinClaw

```bat
winclaw.exe
```

You should see:
```
WinClaw v0.1.1 — Windows-Native Terminal AI Assistant
winclaw[default]>
```

Type any message and press Enter to chat. Type `/help` to see all commands.

---

### Optional — add winclaw.exe to your PATH

To run `winclaw` from any directory:

1. Copy `winclaw.exe` to a permanent location, e.g. `C:\Tools\winclaw.exe`
2. Open **System Properties** → **Advanced** → **Environment Variables**
3. Under **User variables**, select `Path` and click **Edit**
4. Add `C:\Tools` (or wherever you placed the `.exe`)
5. Click OK, then **close and reopen** your terminal

---

### Troubleshooting

**`go: command not found`**
Go is not in your PATH. Close and reopen the terminal after installation. If it still fails, re-run the Go installer and ensure "Add to PATH" is checked.

**`WinClaw: API key not found in Credential Manager`**
Run `winclaw.exe --setup` again. If the setup wizard completed without error the first time, open Credential Manager and check that `WinClaw/AnthropicAPIKey` exists under Windows Credentials.

**`WinClaw refuses to start with an unlocked data directory`**
This means `SetNamedSecurityInfoW` returned an error when applying ACLs to `%APPDATA%\WinClaw`. This is rare and usually means the directory was created by a different user account. Delete `%APPDATA%\WinClaw` and run `winclaw.exe --setup` again.

**Garbled output / no colours**
WinClaw requires a terminal that supports ANSI escape codes. Use Windows Terminal, PowerShell 7+, or Command Prompt on Windows 10 1903+. The legacy `conhost.exe` on older systems may need Virtual Terminal Processing enabled manually.

**`build.bat is not recognized` in PowerShell**
PowerShell does not run scripts from the current directory by default. Use `.\build.bat` instead of `build.bat`. The same applies to the built binary: `.\winclaw.exe` or `.\winclaw.exe --setup`.

**`build.bat` says `go mod tidy failed`**
Usually a network issue downloading dependencies. Try:
```bat
set GOPROXY=direct
go mod tidy
```

---

## Usage

### Terminal commands

| Command | Description |
|---|---|
| `/help` | List all commands |
| `/new [name]` | Create a new session |
| `/sessions` | List all sessions |
| `/switch <id>` | Switch to a session |
| `/delete <id>` | Delete a session |
| `/reset` | Clear conversation history (keeps memory file) |
| `/memory` | Show the current session's MEMORY.md |
| `/memory edit` | Open MEMORY.md in Notepad |
| `/schedule list` | List scheduled tasks |
| `/schedule add <name> <cron> <prompt>` | Add a scheduled task |
| `/schedule pause <id>` | Pause a task |
| `/schedule resume <id>` | Resume a task |
| `/schedule cancel <id>` | Cancel a task |
| `/status` | Session info and running token usage |
| `/exit` | Quit |

### Key bindings

| Key | Action |
|---|---|
| `Up` / `Down` | Navigate input history |
| `Left` / `Right` | Move cursor within line |
| `Ctrl+C` | Cancel in-progress request (or exit if idle) |
| `Ctrl+D` | Exit cleanly |
| `Ctrl+L` | Clear screen |
| `\` at end of line | Continue input on next line |

### Schedule syntax

```
# 5-field cron (min hour day-of-month month day-of-week)
/schedule add daily-brief "0 9 * * 1-5" "Summarise yesterday's work"

# Interval
/schedule add check-in "@every 2h" "What should I focus on next?"

# Once
/schedule add reminder "@once" "Remind me to review the deploy"
```

---

## Configuration

Location: `%APPDATA%\WinClaw\config.json`

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
| `model` | `claude-sonnet-4-6` | Anthropic model identifier |
| `max_tokens` | `8192` | Maximum tokens per response |
| `history_window` | `20` | Conversation turns kept in API context |
| `max_concurrent_agents` | `4` | Maximum parallel scheduled tasks |
| `agent_timeout_seconds` | `300` | Per-request deadline in seconds |
| `stream_responses` | `true` | Stream tokens to terminal as they arrive |
| `log_level` | `info` | `debug` / `info` / `warning` / `error` |

---

## Data layout

```
%APPDATA%\WinClaw\
├── config.json                 ← settings (no secrets)
├── winclaw.db                  ← SQLite: sessions, messages, tasks, audit log
└── sessions\
    └── <session-uuid>\
        └── MEMORY.md           ← per-session context, injected into system prompt
```

All directories are created with explicit per-user ACLs at startup. ACL failure is fatal — WinClaw will not run with an unlocked data directory.

---

## Windows Event Log

WinClaw writes to the Application event log under the source `WinClaw`.

To view: `Win+R` → `eventvwr.msc` → **Windows Logs** → **Application** → filter by source `WinClaw`.

Security events use Event ID 4:
```
SECURITY | action=startup subject=winclaw detail=version=v0.1.1
```

If the event source is not yet registered (first run without elevation), all output falls back to stderr transparently.

---

## Build flags

**Command Prompt:**
```bat
build.bat           :: debug
build.bat release   :: stripped, smaller
```

**PowerShell:**
```powershell
.\build.bat           # debug
.\build.bat release   # stripped, smaller
```

Manual build (either shell):
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
│   │   ├── models.go            ← Anthropic API request/response structs
│   │   └── ratelimit.go         ← token-bucket rate limiter (50 req/min)
│   ├── config/
│   │   └── config.go            ← config load/save with atomic write
│   ├── db/
│   │   ├── db.go                ← SQLite open, WAL pragmas, migration runner
│   │   └── schema.go            ← DDL as Go string constants
│   ├── logging/
│   │   └── eventlog.go          ← Windows Event Log with stderr fallback
│   ├── memory/
│   │   └── memory.go            ← per-session MEMORY.md read/write (RWMutex)
│   ├── scheduler/
│   │   └── scheduler.go         ← cron / @every / @once task scheduler
│   ├── security/
│   │   ├── acl.go               ← Windows ACL (SetNamedSecurityInfoW)
│   │   └── credman.go           ← Windows Credential Manager (DPAPI)
│   └── terminal/
│       └── repl.go              ← raw-mode REPL, line editor, spinner, ANSI
├── build.bat                    ← build script (tidy → vet → build)
├── go.mod
└── CHANGELOG.md
```

---

## Comparison

| | WinClaw | NanoClaw | OpenClaw |
|---|---|---|---|
| **Language** | Go | TypeScript | TypeScript |
| **Runtime** | None — single binary | Node.js | Node.js |
| **Secret storage** | Windows Credential Manager | `.env` files | `.env` files |
| **Interface** | Terminal only | Multi-channel messaging | Multi-channel + web UI |
| **Audit log** | Windows Event Log + SQLite | Console | Console |
| **Startup time** | ~50 ms | ~2–3 s | ~3–5 s |
| **Memory footprint** | ~8 MB | ~80–120 MB | ~150–200 MB |
| **Platforms** | Windows only | macOS / Linux | macOS / Linux / Windows |

---

## License

MIT
