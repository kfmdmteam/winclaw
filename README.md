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
- **Windows-native tools.** Screenshot, process management, registry access, toast notifications, and UAC elevation — using native Windows APIs, not Unix shims.

---

## Security Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│  Windows Credential Manager (DPAPI)                             │
│  └── WinClaw/AnthropicAPIKey  ← never written to disk           │
└──────────────────────┬──────────────────────────────────────────┘
                       │ read at startup, zeroed on exit
┌──────────────────────▼──────────────────────────────────────────┐
│  winclaw.exe  (single process, no root required)                │
│                                                                 │
│  ┌─────────────────────┐      ┌────────────────────────┐        │
│  │  Terminal REPL      │      │  Scheduler             │        │
│  │  (raw mode, no GUI) │      │  (cron / interval)     │        │
│  └──────────┬──────────┘      └───────────┬────────────┘        │
│             └──────────────┬──────────────┘                     │
│                    ┌───────▼────────────────────────────────┐   │
│                    │  Agent  (windowed history · soul file · │   │
│                    │          session + global memory)       │   │
│                    └───────────────┬────────────────────────┘   │
│                                    │ TLS 1.2+ outbound only      │
└────────────────────────────────────┼────────────────────────────┘
                                     ▼
                          api.anthropic.com/v1/messages

 ── Service mode ──────────────────────────────────────────────────
  winclaw.exe --install-service  (run as administrator, once)
  ↓
  Windows SCM → winclaw.exe (LocalService account)
                 └── Named pipe \\.\pipe\WinClaw
                      ← winclaw.exe --send "prompt"
                        (any terminal, same user)
```

### Security controls

| Layer | Control |
|---|---|
| **Secrets (interactive)** | Windows Credential Manager (DPAPI, user-scope) — no `.env`, no plaintext |
| **Secrets (service)** | Machine-DPAPI blob in `service-key.enc`, ACL'd to user + LocalService only |
| **Data directory** | ACL: deny Everyone, grant current user + SYSTEM (`GENERIC_ALL`) — failure is fatal |
| **Service data directory** | ACL upgraded at install time to also grant LocalService |
| **Named pipe** | DACL: SYSTEM, LocalService, and the installing-user SID only — no anonymous connections |
| **Audit trail** | Windows Event Log (Application, Source: WinClaw) + `audit_log` SQLite table |
| **Network** | Outbound HTTPS only, TLS 1.2 minimum, no listening ports |
| **Binary** | Built with `-s -w` (stripped), `//go:build windows` enforced |
| **Database** | SQLite WAL, foreign keys enforced, file created with `0600` permissions |
| **Memory files** | Written atomically (`tmp` + rename), `0600` permissions |
| **API key in memory** | Stored as `[]byte`, zeroed via `client.Close()` on exit |
| **Input validation** | Session names ≤128 chars, prompts ≤10,000 chars, schedules ≤64 chars |
| **PS injection** | User-supplied strings embedded in PowerShell single-quoted strings are sanitised with `psEscape()` |

---

## Token Efficiency

Keeping API costs low is a first-class concern. WinClaw applies three layers of optimisation on every request:

**1. Sliding history window**
Only the most recent `history_window` turns (default: 20) are sent to the API. The full conversation is retained locally for audit, but older messages are excluded from the wire payload. On a long session this can reduce input tokens by 60–80%.

**2. Compact system prompt**
The system prompt is built from three sources, each injected only when non-empty: the soul file (persistent identity), global cross-session memory, and per-session memory. There is no boilerplate padding.

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
```

---

### Step 5 — Run first-time setup

```bat
winclaw.exe --setup
```

The wizard will:

1. Prompt for your Anthropic API key — input is visible so that paste works reliably across all Windows terminals; the key goes directly to Credential Manager and never touches disk
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
  ╭──────────────────────────────────────────╮
  │                                          │
  │   WinClaw v0.2.0                         │
  │   Windows-Native Terminal AI Assistant  │
  │                                          │
  ╰──────────────────────────────────────────╯
  model   · claude-sonnet-4-6
  session · default

  /help for commands · Ctrl+D to exit

[default] ❯
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
| `/global` | Show the global cross-session memory file |
| `/soul` | Show the soul file (persistent identity) |
| `/soul edit` | Open the soul file in Notepad |
| `/think` | Toggle extended thinking mode (deeper reasoning, slower) |
| `/attach <path>` | Attach an image file to your next message |
| `/schedule list` | List scheduled tasks |
| `/schedule add <name> <cron> <prompt>` | Add a scheduled task |
| `/schedule pause <id>` | Pause a task |
| `/schedule resume <id>` | Resume a task |
| `/schedule cancel <id>` | Cancel a task |
| `/status` | Session info, token usage, and model settings |
| `/exit` | Quit |

### Command-line flags

| Flag | Description |
|---|---|
| `--setup` | Run the first-time setup wizard |
| `--version` | Print version and exit |
| `--session <id>` | Resume a specific session by ID |
| `--model <id>` | Override the model (e.g. `claude-opus-4-6`) |
| `--no-color` | Disable ANSI colour output |
| `--log-level <level>` | Set log level: `debug`, `info`, `warning`, `error` |
| `--attach <path>` | Attach an image to the initial prompt |
| `--send "<prompt>"` | Send a prompt to the running service and stream the response |
| `--install-service` | Register WinClaw as a Windows Service (requires administrator) |
| `--uninstall-service` | Remove the WinClaw Windows Service (requires administrator) |
| `--start-service` | Start the WinClaw service via SCM |
| `--stop-service` | Stop the WinClaw service via SCM |
| `--service-status` | Print the service status and exit |

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

## Agent Tools

The agent can call the following tools autonomously. It will describe what it is about to do before executing anything destructive.

### System

| Tool | Description |
|---|---|
| `bash` | Run a PowerShell command and return the output (timeout: 30 s, max 120 s) |
| `run_elevated` | Run a PowerShell command with UAC elevation (triggers a UAC prompt) |

### Files

| Tool | Description |
|---|---|
| `read_file` | Read a file from the filesystem (truncated at 20 KB) |
| `write_file` | Write or overwrite a file |
| `list_directory` | List directory contents with sizes and timestamps |

### Web

| Tool | Description |
|---|---|
| `web_search` | Search via the DuckDuckGo Instant Answer API (no key required) |
| `fetch_url` | Fetch and strip a URL to plain text (truncated at 12 KB) |

### Windows-native

| Tool | Description |
|---|---|
| `screenshot` | Capture the primary monitor as PNG for visual analysis |
| `process_list` | List running processes by CPU / memory (top 50, or filtered by name) |
| `kill_process` | Terminate a process by PID or name |
| `toast_notify` | Send a Windows desktop balloon notification |
| `registry_read` | Read a Windows registry value or key |
| `registry_write` | Write a Windows registry value |

### Memory

| Tool | Description |
|---|---|
| `update_memory` | Append a note to the current session's MEMORY.md |
| `update_global_memory` | Append a note to the cross-session GLOBAL.md |
| `update_soul` | Rewrite the soul file (persistent identity and values) |

### Advanced

| Tool | Description |
|---|---|
| `delegate` | Spawn a fresh sub-agent with a clean context to handle an isolated sub-task |

### Plugins

Any `.ps1` file placed in `%APPDATA%\WinClaw\plugins\` that starts with a `# WinClaw-Plugin` header block is automatically loaded as an additional tool. See [Plugin system](#plugin-system) below.

---

## Plugin System

Drop a `.ps1` file in `%APPDATA%\WinClaw\plugins\` with this header:

```powershell
# WinClaw-Plugin
# Name: my_tool
# Description: Does something useful.
# Parameters: {"type":"object","properties":{"input":{"type":"string","description":"The input"}},"required":["input"]}

param(
    [string]$InputJson = "{}"
)

$args = $InputJson | ConvertFrom-Json
# ... your logic here ...
Write-Output "Result: $($args.input)"
```

WinClaw scans the plugins directory at startup. Each valid plugin appears as an agent tool named by the `Name:` field. The agent calls it with `-InputJson '<json>'`; your script receives the parameters and writes results to stdout.

---

## Windows Service Mode

WinClaw can run as a Windows Service under `NT AUTHORITY\LocalService`, keeping the scheduler alive in the background and accepting prompts from any terminal via a named pipe.

### Install

Run as **administrator** (once):

```bat
winclaw.exe --install-service
```

This:
1. Re-encrypts the API key with machine-scope DPAPI into `service-key.enc`
2. Locks `service-key.enc` to your user SID + LocalService only
3. Updates the data directory ACL to grant LocalService access
4. Registers the `WinClaw` service with the SCM (auto-start, LocalService account)

### Control

```bat
winclaw.exe --start-service
winclaw.exe --service-status
winclaw.exe --stop-service
winclaw.exe --uninstall-service
```

### Send prompts to the running service

```bat
winclaw.exe --send "Summarise the last 24 hours of Event Log errors"
```

The response streams to stdout. Combine with `--session <id>` to target a specific session. The named pipe (`\\.\pipe\WinClaw`) is ACL'd to SYSTEM, LocalService, and the installing user only.

---

## Memory and Identity

WinClaw has three persistent text stores that are injected into the system prompt on every turn:

| File | Location | Purpose |
|---|---|---|
| `SOUL.md` | `%APPDATA%\WinClaw\SOUL.md` | Persistent identity, values, and self-knowledge. Written once at first run; the agent can rewrite it with `update_soul`. |
| `GLOBAL.md` | `%APPDATA%\WinClaw\GLOBAL.md` | Cross-session facts (user preferences, project context). The agent appends to it with `update_global_memory`. |
| `MEMORY.md` | `%APPDATA%\WinClaw\sessions\<id>\MEMORY.md` | Per-session notes. The agent appends with `update_memory`. Auto-consolidated when it exceeds 8 KB. |

The injection order in the system prompt is: soul → date → environment → global memory → session memory.

---

## Extended Thinking

Toggle deeper reasoning for the current session:

```
[default] ❯ /think
Extended thinking enabled (budget: 10000 tokens). Responses will be deeper and slower.
```

When enabled, each request includes `"thinking": {"type":"enabled","budget_tokens":10000}` and the appropriate beta header. The thinking content is streamed but not shown in the terminal — only the final text response is displayed. Toggle off with `/think` again.

Extended thinking increases latency and token cost significantly. Use it for complex architectural decisions, difficult debugging, or multi-step reasoning tasks.

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
├── SOUL.md                     ← persistent identity (injected into every prompt)
├── GLOBAL.md                   ← cross-session memory (injected into every prompt)
├── service-key.enc             ← machine-DPAPI key blob (service mode only)
└── sessions\
    └── <session-uuid>\
        └── MEMORY.md           ← per-session context, injected into system prompt
└── plugins\
    └── *.ps1                   ← optional PowerShell tool plugins
```

All directories are created with explicit per-user ACLs at startup. ACL failure is fatal — WinClaw will not run with an unlocked data directory.

---

## Windows Event Log

WinClaw writes to the Application event log under the source `WinClaw`.

To view: `Win+R` → `eventvwr.msc` → **Windows Logs** → **Application** → filter by source `WinClaw`.

Security events use Event ID 4:
```
SECURITY | action=startup subject=winclaw detail=version=v0.2.0
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
go build -tags windows -ldflags="-s -w -X main.version=v0.2.0" -o winclaw.exe ./cmd/winclaw
```

---

## Project layout

```
winclaw/
├── cmd/winclaw/main.go          ← entry point, setup wizard, service detection, flags
├── internal/
│   ├── agent/
│   │   ├── agent.go             ← run loop, windowed history, thinking, attachment, auto-consolidation
│   │   └── session.go           ← session CRUD, message audit log
│   ├── api/
│   │   ├── client.go            ← HTTP client, SSE streaming, retry/backoff
│   │   ├── models.go            ← Anthropic API request/response structs (incl. thinking, images)
│   │   └── ratelimit.go         ← token-bucket rate limiter (50 req/min)
│   ├── config/
│   │   └── config.go            ← config load/save with atomic write
│   ├── db/
│   │   ├── db.go                ← SQLite open, WAL pragmas, migration runner
│   │   └── schema.go            ← DDL as Go string constants
│   ├── ipc/
│   │   └── pipe.go              ← named pipe server/client, length-prefixed JSON frames
│   ├── logging/
│   │   └── eventlog.go          ← Windows Event Log with stderr fallback
│   ├── memory/
│   │   └── memory.go            ← MEMORY.md (session), GLOBAL.md, SOUL.md — all RWMutex-safe
│   ├── scheduler/
│   │   └── scheduler.go         ← cron / @every / @once task scheduler
│   ├── security/
│   │   ├── acl.go               ← Windows ACL (SetNamedSecurityInfoW, LockDirToCurrentUser, LockDirToService)
│   │   ├── credman.go           ← Windows Credential Manager (DPAPI, user-scope)
│   │   └── dpapi.go             ← CryptProtectData / CryptUnprotectData (machine-scope)
│   ├── service/
│   │   ├── install.go           ← SCM install / uninstall / start / stop / status
│   │   └── service.go           ← svc.Handler: startup, scheduler, pipe server, SCM loop
│   ├── terminal/
│   │   └── repl.go              ← raw-mode REPL, line editor, spinner, ANSI, slash commands
│   └── tools/
│       ├── bash.go              ← executeBash / executePowerShell
│       ├── files.go             ← read_file, write_file, list_directory
│       ├── memory.go            ← update_memory, update_soul
│       ├── plugin.go            ← .ps1 plugin loader and executor
│       ├── registry.go          ← tool definitions and dispatch (Registry)
│       ├── search.go            ← web_search (DuckDuckGo), fetch_url
│       └── windows.go           ← screenshot, process_list, kill_process, toast_notify,
│                                   run_elevated, registry_read, registry_write
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
| **Interface** | Terminal + background service | Multi-channel messaging | Multi-channel + web UI |
| **Audit log** | Windows Event Log + SQLite | Console | Console |
| **Tools** | 17 built-in + .ps1 plugins | Basic | Basic |
| **Startup time** | ~50 ms | ~2–3 s | ~3–5 s |
| **Memory footprint** | ~8 MB | ~80–120 MB | ~150–200 MB |
| **Platforms** | Windows only | macOS / Linux | macOS / Linux / Windows |

---

## Troubleshooting

**`go: command not found`**
Go is not in your PATH. Close and reopen the terminal after installation. If it still fails, re-run the Go installer and ensure "Add to PATH" is checked.

**`WinClaw: API key not found in Credential Manager`**
Run `winclaw.exe --setup` again. If the setup wizard completed without error the first time, open Credential Manager and check that `WinClaw/AnthropicAPIKey` exists under Windows Credentials.

**`WinClaw refuses to start with an unlocked data directory`**
This means `SetNamedSecurityInfoW` returned an error when applying ACLs to `%APPDATA%\WinClaw`. This is rare and usually means the directory was created by a different user account. Delete `%APPDATA%\WinClaw` and run `winclaw.exe --setup` again.

**`--install-service` fails with "Access is denied"**
Service installation requires administrator rights. Right-click your terminal and choose **Run as administrator**, then run `winclaw.exe --install-service`.

**`--send` returns "is the WinClaw service running?"**
The named pipe `\\.\pipe\WinClaw` does not exist. Start the service first: `winclaw.exe --start-service`.

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

## License

MIT
