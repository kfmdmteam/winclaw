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
