package db

// CreateSessions is the DDL for the sessions table, which stores one row per
// conversation session. The deleted column is a soft-delete flag (0 = active,
// 1 = deleted); queries always filter on deleted = 0.
const CreateSessions = `
CREATE TABLE IF NOT EXISTS sessions (
    id          TEXT    PRIMARY KEY,
    name        TEXT    NOT NULL,
    created_at  INTEGER NOT NULL,
    last_active INTEGER NOT NULL,
    memory_path TEXT    NOT NULL,
    deleted     INTEGER NOT NULL DEFAULT 0
);`

// CreateMessages is the DDL for the messages table, which stores every turn
// of every conversation in chronological order.
const CreateMessages = `
CREATE TABLE IF NOT EXISTS messages (
    id          TEXT    PRIMARY KEY,
    session_id  TEXT    NOT NULL REFERENCES sessions(id),
    role        TEXT    NOT NULL CHECK(role IN ('user','assistant','system')),
    content     TEXT    NOT NULL,
    created_at  INTEGER NOT NULL,
    tokens_used INTEGER DEFAULT 0
);`

// CreateMessagesIndex creates a covering index on (session_id, created_at) so
// that fetching all messages for a session in order is a fast range scan.
const CreateMessagesIndex = `
CREATE INDEX IF NOT EXISTS idx_messages_session ON messages(session_id, created_at);`

// CreateScheduledTasks is the DDL for the scheduled_tasks table, which stores
// cron, interval, and one-time tasks to be executed by the scheduler.
const CreateScheduledTasks = `
CREATE TABLE IF NOT EXISTS scheduled_tasks (
    id         TEXT    PRIMARY KEY,
    session_id TEXT    REFERENCES sessions(id),
    name       TEXT    NOT NULL,
    schedule   TEXT    NOT NULL,
    next_run   INTEGER NOT NULL,
    last_run   INTEGER,
    status     TEXT    NOT NULL DEFAULT 'active'
                       CHECK(status IN ('active','paused','cancelled')),
    prompt     TEXT    NOT NULL,
    created_at INTEGER NOT NULL
);`

// CreateAuditLog is the DDL for the audit_log table, which stores an
// append-only record of security-relevant events. The id column is an
// auto-incrementing integer so that event ordering is preserved even if two
// events share the same millisecond timestamp.
const CreateAuditLog = `
CREATE TABLE IF NOT EXISTS audit_log (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    event_time INTEGER NOT NULL,
    action     TEXT    NOT NULL,
    subject    TEXT    NOT NULL,
    detail     TEXT,
    success    INTEGER NOT NULL DEFAULT 1
);`

// CreateAuditLogIndex creates an index on event_time to support time-range
// queries against the audit log.
const CreateAuditLogIndex = `
CREATE INDEX IF NOT EXISTS idx_audit_time ON audit_log(event_time);`

// migrations lists every DDL statement in the order it must be applied.
// Each entry is a standalone statement. RunMigrations in db.go iterates this
// slice and executes each statement inside a transaction.
var migrations = []string{
	CreateSessions,
	CreateMessages,
	CreateMessagesIndex,
	CreateScheduledTasks,
	CreateAuditLog,
	CreateAuditLogIndex,
}
