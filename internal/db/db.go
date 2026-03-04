package db

import (
	"database/sql"
	"fmt"
	"path/filepath"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

const dbFileName = "winclaw.db"

// DB wraps a *sql.DB and carries the schema migration version that was applied
// when the database was last opened.
type DB struct {
	conn             *sql.DB
	migrationVersion int
}

// Open opens (or creates) the WinClaw SQLite database in dataDir. It enables
// WAL journal mode and foreign-key enforcement, then runs any pending schema
// migrations. Returns a ready-to-use *DB or an error.
func Open(dataDir string) (*DB, error) {
	if dataDir == "" {
		return nil, fmt.Errorf("db: dataDir must not be empty")
	}

	dbPath := filepath.Join(dataDir, dbFileName)

	// modernc.org/sqlite uses the driver name "sqlite".
	conn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("db: open %q: %w", dbPath, err)
	}

	// Limit to a single connection to avoid WAL write conflicts from multiple
	// goroutines and to keep memory overhead predictable.
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)

	db := &DB{conn: conn}

	if err := db.applyPragmas(); err != nil {
		_ = conn.Close()
		return nil, err
	}

	if err := db.RunMigrations(); err != nil {
		_ = conn.Close()
		return nil, err
	}

	return db, nil
}

// applyPragmas sets connection-level options that must be configured before
// any tables are created or accessed.
func (db *DB) applyPragmas() error {
	pragmas := []string{
		// WAL mode gives better read/write concurrency than the default
		// DELETE journal mode and is safer under concurrent access.
		`PRAGMA journal_mode=WAL;`,
		// Enforce REFERENCES constraints so orphan rows are impossible.
		`PRAGMA foreign_keys=ON;`,
		// Synchronous=NORMAL is safe with WAL and faster than FULL.
		`PRAGMA synchronous=NORMAL;`,
		// 32 MiB page cache — comfortable for typical WinClaw workloads.
		`PRAGMA cache_size=-32000;`,
		// Store temporary tables in memory for performance.
		`PRAGMA temp_store=MEMORY;`,
	}

	for _, p := range pragmas {
		if _, err := db.conn.Exec(p); err != nil {
			return fmt.Errorf("db: pragma %q: %w", p, err)
		}
	}
	return nil
}

// RunMigrations executes all DDL statements in the migrations slice (defined
// in schema.go) inside a single transaction. The operation is idempotent
// because every statement uses CREATE TABLE IF NOT EXISTS / CREATE INDEX IF
// NOT EXISTS, so re-running against an existing database is safe.
func (db *DB) RunMigrations() error {
	tx, err := db.conn.Begin()
	if err != nil {
		return fmt.Errorf("db: begin migration transaction: %w", err)
	}
	defer func() {
		// If we return an error the transaction will be open; roll it back.
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	for i, stmt := range migrations {
		if _, err = tx.Exec(stmt); err != nil {
			return fmt.Errorf("db: migration[%d]: %w", i, err)
		}
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("db: commit migrations: %w", err)
	}

	db.migrationVersion = len(migrations)
	return nil
}

// MigrationVersion returns the number of migration statements that were
// applied when this DB was opened. This is informational and useful for
// diagnostic logging.
func (db *DB) MigrationVersion() int {
	return db.migrationVersion
}

// Conn exposes the underlying *sql.DB for callers that need to issue queries
// directly. Prefer using higher-level repository types where possible.
func (db *DB) Conn() *sql.DB {
	return db.conn
}

// Close releases all database resources. It is safe to call Close more than
// once; subsequent calls are no-ops.
func (db *DB) Close() error {
	if db.conn == nil {
		return nil
	}
	if err := db.conn.Close(); err != nil {
		return fmt.Errorf("db: close: %w", err)
	}
	db.conn = nil
	return nil
}
