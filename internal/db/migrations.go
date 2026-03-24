package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type migration struct {
	version int
	name    string
	query   string
}

var migrations = []migration{
	{
		version: 1,
		name:    "create projects and reports tables",
		query: `
CREATE TABLE IF NOT EXISTS projects (
    id TEXT PRIMARY KEY,
    slug TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS reports (
    id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('pending', 'processing', 'ready', 'failed')),
    archive_object_key TEXT NOT NULL,
    generated_object_key TEXT NOT NULL DEFAULT '',
    archive_format TEXT NOT NULL DEFAULT '',
    source_filename TEXT NOT NULL DEFAULT '',
    error_message TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    started_at TEXT,
    completed_at TEXT,
    FOREIGN KEY(project_id) REFERENCES projects(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_projects_slug ON projects(slug);
CREATE INDEX IF NOT EXISTS idx_reports_project_id_created_at ON reports(project_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_reports_status ON reports(status);
`,
	},
	{
		version: 2,
		name:    "create api_keys table",
		query: `
CREATE TABLE IF NOT EXISTS api_keys (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    key_hash TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_api_keys_name ON api_keys(name);
CREATE INDEX IF NOT EXISTS idx_api_keys_key_hash ON api_keys(key_hash);
`,
	},
	{
		version: 3,
		name:    "add project retention and report completion indexes",
		query: `
ALTER TABLE projects ADD COLUMN retention_days INTEGER;
CREATE INDEX IF NOT EXISTS idx_projects_retention_days ON projects(retention_days);
CREATE INDEX IF NOT EXISTS idx_reports_status_completed_at ON reports(status, completed_at);
`,
	},
}

func RunMigrations(ctx context.Context, db *sql.DB, busyTimeout time.Duration) error {
	if db == nil {
		return fmt.Errorf("run migrations: nil db")
	}

	if err := applySQLitePragmas(ctx, db, busyTimeout); err != nil {
		return err
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration transaction: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER PRIMARY KEY,
    name TEXT NOT NULL,
    applied_at TEXT NOT NULL
);
`); err != nil {
		return fmt.Errorf("create schema_migrations table: %w", err)
	}

	applied, err := loadAppliedMigrations(ctx, tx)
	if err != nil {
		return err
	}

	for _, migration := range migrations {
		if applied[migration.version] {
			continue
		}

		if _, err := tx.ExecContext(ctx, migration.query); err != nil {
			return fmt.Errorf("apply migration %d (%s): %w", migration.version, migration.name, err)
		}

		if _, err := tx.ExecContext(ctx, `
INSERT INTO schema_migrations (version, name, applied_at)
VALUES (?, ?, ?)
`, migration.version, migration.name, formatTimestamp(nowUTC())); err != nil {
			return fmt.Errorf("record migration %d (%s): %w", migration.version, migration.name, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migrations: %w", err)
	}

	return nil
}

func applySQLitePragmas(ctx context.Context, db *sql.DB, busyTimeout time.Duration) error {
	if _, err := db.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout = %d", busyTimeout.Milliseconds())); err != nil {
		return fmt.Errorf("set sqlite busy_timeout: %w", err)
	}

	var journalMode string
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode = WAL`).Scan(&journalMode); err != nil {
		return fmt.Errorf("set sqlite journal_mode WAL: %w", err)
	}

	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
		return fmt.Errorf("enable sqlite foreign_keys: %w", err)
	}

	if _, err := db.ExecContext(ctx, `PRAGMA synchronous = NORMAL`); err != nil {
		return fmt.Errorf("set sqlite synchronous NORMAL: %w", err)
	}

	return nil
}

func loadAppliedMigrations(ctx context.Context, tx *sql.Tx) (map[int]bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("query applied migrations: %w", err)
	}
	defer rows.Close()

	applied := make(map[int]bool, len(migrations))
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			return nil, fmt.Errorf("scan applied migration: %w", err)
		}
		applied[version] = true
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate applied migrations: %w", err)
	}

	return applied, nil
}
