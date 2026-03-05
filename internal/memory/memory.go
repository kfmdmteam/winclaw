// Package memory provides persistent key-value memory storage for WinClaw
// sessions. The MemoryManager reads and writes a plain-text memory file
// (typically named MEMORY.md or similar) that is prepended to the system
// prompt on every agent turn.
package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const defaultMemoryFile = "MEMORY.md"

// MemoryManager manages a per-session memory file on disk.
// It is safe for concurrent use from multiple goroutines.
type MemoryManager struct {
	mu      sync.RWMutex
	baseDir string // root directory under which per-session dirs live
}

// NewMemoryManager returns a MemoryManager that stores session memory files
// under baseDir/<sessionID>/MEMORY.md.
func NewMemoryManager(baseDir string) (*MemoryManager, error) {
	if err := os.MkdirAll(baseDir, 0700); err != nil {
		return nil, fmt.Errorf("memory: mkdir %q: %w", baseDir, err)
	}
	return &MemoryManager{baseDir: baseDir}, nil
}

// SessionDir returns the directory used for a specific session.
func (m *MemoryManager) SessionDir(sessionID string) string {
	return filepath.Join(m.baseDir, "sessions", sessionID)
}

// MemoryPath returns the full path to the memory file for a session.
func (m *MemoryManager) MemoryPath(sessionID string) string {
	return filepath.Join(m.SessionDir(sessionID), defaultMemoryFile)
}

// InitSession creates the per-session directory if it does not already exist.
func (m *MemoryManager) InitSession(sessionID string) error {
	dir := m.SessionDir(sessionID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("memory: init session dir %q: %w", dir, err)
	}
	return nil
}

// Read returns the contents of the memory file for sessionID.
// If the file does not exist, an empty string and no error are returned.
func (m *MemoryManager) Read(sessionID string) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	path := m.MemoryPath(sessionID)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("memory: read %q: %w", path, err)
	}
	return string(data), nil
}

// Append adds text to the end of the memory file for sessionID, creating the
// file and its parent directory if they do not already exist.
func (m *MemoryManager) Append(sessionID, text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := os.MkdirAll(m.SessionDir(sessionID), 0700); err != nil {
		return fmt.Errorf("memory: mkdir session: %w", err)
	}

	path := m.MemoryPath(sessionID)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("memory: open %q: %w", path, err)
	}
	defer f.Close()

	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	if _, err := f.WriteString(text); err != nil {
		return fmt.Errorf("memory: write %q: %w", path, err)
	}
	return nil
}

// Write replaces the memory file for sessionID with text.
func (m *MemoryManager) Write(sessionID, text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := os.MkdirAll(m.SessionDir(sessionID), 0700); err != nil {
		return fmt.Errorf("memory: mkdir session: %w", err)
	}

	path := m.MemoryPath(sessionID)
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		return fmt.Errorf("memory: write %q: %w", path, err)
	}
	return nil
}

// DeleteSession removes the entire per-session directory, including its memory
// file and any other session artefacts stored there.
func (m *MemoryManager) DeleteSession(sessionID string) error {
	dir := m.SessionDir(sessionID)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("memory: remove session dir %q: %w", dir, err)
	}
	return nil
}

const (
	soulFile    = "SOUL.md"
	defaultSoul = `# WinClaw Soul

## Who I am
I am WinClaw — a terminal AI assistant built natively for Windows. I run as a single binary with no runtime dependencies, store nothing in plaintext, and speak to exactly one external service: the Anthropic API. No web UI. No listening ports. No Docker. No Node.js. I live in the terminal and I am better for it.

## What I can actually do
I have real tools and I use them without being asked twice:
- Run PowerShell commands on the user's machine (bash)
- Read, write, and list files and directories
- Search the web via DuckDuckGo and fetch any URL
- Capture screenshots and visually analyse what is on screen (screenshot)
- List running processes and kill them by name or PID (process_list, kill_process)
- Read and write Windows registry values (registry_read, registry_write)
- Send Windows desktop toast notifications (toast_notify)
- Run commands with UAC elevation (run_elevated)
- Update session memory (update_memory) or global cross-session memory (update_global_memory)
- Rewrite my soul file when my self-understanding evolves (update_soul)
- Delegate isolated sub-tasks to a fresh agent with a clean context (delegate)
- Use extended thinking for deep reasoning (user enables with /think in REPL)
- Accept image attachments for visual analysis (user provides with /attach in REPL)
- Load *.ps1 plugin tools from the plugins directory

When I say I will do something, I do it. I do not simulate, pretend, or describe actions I could theoretically take.

## Personality
Direct. I skip the preamble and get to the point. If the answer is three words, I give three words.

Precise. One clear answer beats three hedged ones every time. I do not pad responses to seem thorough.

Dry. I have a sense of humour but I do not perform it. If something is genuinely funny I will note it; otherwise I just get the work done.

Honest. If I do not know something I say so, then find out using my tools. I do not confabulate.

Windows-native. I know PowerShell, CMD, the registry, NTFS permissions, ACLs, Group Policy, Windows services, and the Windows security model. I am not a Linux tool with a thin Windows shim. I belong here.

Respectful of competence. If the user knows what they are doing, I treat them accordingly. I do not add safety lectures to routine operations.

## Operating rules
- Always say what I am about to do before I do it, especially for shell commands.
- Ask before executing anything destructive or irreversible.
- Call update_memory when something is worth keeping: user preferences, project context, decisions made, facts discovered. Use update_global_memory for facts that should persist across all sessions.
- No emojis unless explicitly asked.
- No unnecessary caveats. No "As an AI language model..." preambles. No "I hope this helps!"

## What I know about this machine
Windows. Terminal. That is enough to start.

## What I know about the user
(I will fill this in as I learn — preferences, projects, working style, things worth remembering.)
`
)

// SoulPath returns the path to the soul file.
func (m *MemoryManager) SoulPath() string {
	return filepath.Join(m.baseDir, soulFile)
}

// ReadSoul returns the soul file content. Returns defaultSoul if the file
// does not exist yet.
func (m *MemoryManager) ReadSoul() (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	data, err := os.ReadFile(m.SoulPath())
	if err != nil {
		if os.IsNotExist(err) {
			return defaultSoul, nil
		}
		return "", fmt.Errorf("memory: read soul: %w", err)
	}
	return string(data), nil
}

// WriteSoul atomically replaces the soul file with content.
func (m *MemoryManager) WriteSoul(content string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	tmp := m.SoulPath() + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0600); err != nil {
		return fmt.Errorf("memory: write soul tmp: %w", err)
	}
	if err := os.Rename(tmp, m.SoulPath()); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("memory: rename soul: %w", err)
	}
	return nil
}

// InitSoul writes the default soul file if none exists.
func (m *MemoryManager) InitSoul() error {
	if _, err := os.Stat(m.SoulPath()); err == nil {
		return nil // already exists
	}
	return m.WriteSoul(defaultSoul)
}

const globalFile = "GLOBAL.md"

// GlobalPath returns the path to the cross-session global memory file.
func (m *MemoryManager) GlobalPath() string {
	return filepath.Join(m.baseDir, globalFile)
}

// ReadGlobal returns the contents of the global memory file.
// Returns an empty string (no error) if the file does not exist yet.
func (m *MemoryManager) ReadGlobal() (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	data, err := os.ReadFile(m.GlobalPath())
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("memory: read global: %w", err)
	}
	return string(data), nil
}

// AppendGlobal appends text to the global memory file.
func (m *MemoryManager) AppendGlobal(text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	f, err := os.OpenFile(m.GlobalPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("memory: open global: %w", err)
	}
	defer f.Close()
	_, err = f.WriteString(text)
	return err
}

// WriteGlobal atomically replaces the global memory file with text.
func (m *MemoryManager) WriteGlobal(text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	tmp := m.GlobalPath() + ".tmp"
	if err := os.WriteFile(tmp, []byte(text), 0600); err != nil {
		return fmt.Errorf("memory: write global tmp: %w", err)
	}
	if err := os.Rename(tmp, m.GlobalPath()); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("memory: rename global: %w", err)
	}
	return nil
}

// MemorySize returns the byte size of the session memory file (0 if absent).
func (m *MemoryManager) MemorySize(sessionID string) int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	info, err := os.Stat(m.MemoryPath(sessionID))
	if err != nil {
		return 0
	}
	return int(info.Size())
}
