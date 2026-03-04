package agent

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"winclaw/internal/api"
	"winclaw/internal/memory"
)

// Session holds the runtime state of a single conversation session.
// The in-memory Messages slice is the live conversation context sent to the
// model. Saving every message to DB on receipt would create unnecessary round-
// trips; instead, callers persist individual messages via SaveMessage.
type Session struct {
	ID         string
	Name       string
	CreatedAt  time.Time
	LastActive time.Time
	MemoryPath string

	// Messages is the in-memory conversation history passed to the API.
	// It is NOT persisted to the database per message for performance reasons;
	// the DB copy is the append-only audit log written by SaveMessage.
	Messages []api.Message
}

// SessionManager creates, loads, lists, and deletes sessions using a SQLite
// database and a MemoryManager for per-session file storage.
type SessionManager struct {
	db     *sql.DB
	memory *memory.MemoryManager
}

// NewSessionManager creates a SessionManager. The caller is responsible for
// ensuring that the sessions table exists in db before calling any methods.
func NewSessionManager(db *sql.DB, mem *memory.MemoryManager) *SessionManager {
	return &SessionManager{db: db, memory: mem}
}

// Create inserts a new session record, initialises the per-session memory
// directory, and returns the populated Session.
func (sm *SessionManager) Create(name string) (*Session, error) {
	id := uuid.New().String()
	now := time.Now().UTC()

	memPath := sm.memory.MemoryPath(id)
	if err := sm.memory.InitSession(id); err != nil {
		return nil, fmt.Errorf("session: init memory dir: %w", err)
	}

	const q = `
		INSERT INTO sessions (id, name, created_at, last_active, memory_path, deleted)
		VALUES (?, ?, ?, ?, ?, 0)`
	if _, err := sm.db.Exec(q, id, name, now.Unix(), now.Unix(), memPath); err != nil {
		return nil, fmt.Errorf("session: insert %q: %w", id, err)
	}

	return &Session{
		ID:         id,
		Name:       name,
		CreatedAt:  now,
		LastActive: now,
		MemoryPath: memPath,
		Messages:   []api.Message{},
	}, nil
}

// Load retrieves a session from the database by ID.
// It returns an error if the session does not exist or has been soft-deleted.
func (sm *SessionManager) Load(id string) (*Session, error) {
	const q = `
		SELECT id, name, created_at, last_active, memory_path
		FROM sessions
		WHERE id = ? AND deleted = 0`

	var (
		sess      Session
		createdAt int64
		lastActive int64
	)
	err := sm.db.QueryRow(q, id).Scan(
		&sess.ID,
		&sess.Name,
		&createdAt,
		&lastActive,
		&sess.MemoryPath,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("session: not found: %q", id)
		}
		return nil, fmt.Errorf("session: load %q: %w", id, err)
	}

	sess.CreatedAt = time.Unix(createdAt, 0).UTC()
	sess.LastActive = time.Unix(lastActive, 0).UTC()
	sess.Messages = []api.Message{}
	return &sess, nil
}

// List returns all non-deleted sessions, most recently active first.
func (sm *SessionManager) List() ([]*Session, error) {
	const q = `
		SELECT id, name, created_at, last_active, memory_path
		FROM sessions
		WHERE deleted = 0
		ORDER BY last_active DESC`

	rows, err := sm.db.Query(q)
	if err != nil {
		return nil, fmt.Errorf("session: list: %w", err)
	}
	defer rows.Close()

	var sessions []*Session
	for rows.Next() {
		var (
			sess       Session
			createdAt  int64
			lastActive int64
		)
		if err := rows.Scan(
			&sess.ID,
			&sess.Name,
			&createdAt,
			&lastActive,
			&sess.MemoryPath,
		); err != nil {
			return nil, fmt.Errorf("session: scan row: %w", err)
		}
		sess.CreatedAt = time.Unix(createdAt, 0).UTC()
		sess.LastActive = time.Unix(lastActive, 0).UTC()
		sess.Messages = []api.Message{}
		sessions = append(sessions, &sess)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("session: list rows: %w", err)
	}

	return sessions, nil
}

// Delete soft-deletes a session and removes its on-disk directory tree.
func (sm *SessionManager) Delete(id string) error {
	const q = `UPDATE sessions SET deleted = 1 WHERE id = ?`
	res, err := sm.db.Exec(q, id)
	if err != nil {
		return fmt.Errorf("session: delete %q: %w", id, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("session: not found: %q", id)
	}

	// Remove session directory; ignore error if already absent.
	if err := sm.memory.DeleteSession(id); err != nil {
		return fmt.Errorf("session: remove memory dir: %w", err)
	}
	return nil
}

// UpdateLastActive sets the last_active timestamp for a session to now.
func (sm *SessionManager) UpdateLastActive(id string) error {
	const q = `UPDATE sessions SET last_active = ? WHERE id = ? AND deleted = 0`
	res, err := sm.db.Exec(q, time.Now().UTC().Unix(), id)
	if err != nil {
		return fmt.Errorf("session: update last_active %q: %w", id, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("session: not found: %q", id)
	}
	return nil
}

// SaveMessage appends a message to the session's DB audit log.
// tokensUsed is the number of tokens charged for this message (0 for user turns).
func (sm *SessionManager) SaveMessage(sessionID, role, content string, tokensUsed int) error {
	const q = `
		INSERT INTO messages (id, session_id, role, content, tokens_used, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`
	id := uuid.New().String()
	now := time.Now().UTC().Unix()
	if _, err := sm.db.Exec(q, id, sessionID, role, content, tokensUsed, now); err != nil {
		return fmt.Errorf("session: save message for %q: %w", sessionID, err)
	}
	return nil
}

