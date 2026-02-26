package db

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaDDL string

const (
	currentSchemaVersion = 23
	maxSchemaVersion     = 23
)

var (
	ErrSchemaVersionTooNew = errors.New("database schema version is newer than this binary supports — upgrade intercore")
	ErrNotMigrated         = errors.New("database not migrated — run 'ic init'")
)

// DB wraps a SQLite database connection for intercore.
type DB struct {
	db   *sql.DB
	path string
}

// Open opens or creates an intercore database at path.
func Open(path string, busyTimeout time.Duration) (*DB, error) {
	// Symlink check on parent directory
	dir := filepath.Dir(path)
	if info, err := os.Lstat(dir); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("open: %s is a symlink (refusing to create DB)", dir)
	}

	if busyTimeout <= 0 {
		busyTimeout = 5 * time.Second
	}

	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode%%3DWAL&_pragma=busy_timeout%%3D%d", path, busyTimeout.Milliseconds())
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}

	// Single connection prevents WAL checkpoint TOCTOU
	sqlDB.SetMaxOpenConns(1)

	// Force connection init and set PRAGMAs explicitly
	// (DSN _pragma may not be applied reliably on all driver versions)
	if _, err := sqlDB.Exec(fmt.Sprintf("PRAGMA busy_timeout = %d", busyTimeout.Milliseconds())); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("open: set busy_timeout: %w", err)
	}
	if _, err := sqlDB.Exec("PRAGMA journal_mode = WAL"); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("open: set WAL: %w", err)
	}
	if _, err := sqlDB.Exec("PRAGMA foreign_keys = ON"); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("open: set foreign_keys: %w", err)
	}

	// Centralized schema version check on every Open
	var version int
	if err := sqlDB.QueryRow("PRAGMA user_version").Scan(&version); err == nil {
		if version > maxSchemaVersion {
			sqlDB.Close()
			return nil, ErrSchemaVersionTooNew
		}
	}

	return &DB{db: sqlDB, path: path}, nil
}

// SqlDB returns the underlying *sql.DB for use by store packages.
func (d *DB) SqlDB() *sql.DB {
	return d.db
}

// Close closes the database.
func (d *DB) Close() error {
	return d.db.Close()
}

// SchemaVersion returns the current PRAGMA user_version.
func (d *DB) SchemaVersion() (int, error) {
	var v int
	err := d.db.QueryRow("PRAGMA user_version").Scan(&v)
	return v, err
}

// Migrate applies the schema DDL if needed.
// Creates a timestamped backup before any migration attempt.
func (d *DB) Migrate(ctx context.Context) error {
	// Create backup before migration if DB file exists
	if info, err := os.Stat(d.path); err == nil && info.Size() > 0 {
		backupPath := fmt.Sprintf("%s.backup-%s", d.path, time.Now().Format("20060102-150405"))
		if err := copyFile(d.path, backupPath); err != nil {
			return fmt.Errorf("migrate: backup failed: %w", err)
		}
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migrate: begin: %w", err)
	}
	defer tx.Rollback()

	// Acquire exclusive lock by writing to a temp table.
	// A deferred transaction only acquires a shared lock on first read;
	// we need exclusive to prevent two concurrent migrations.
	if _, err := tx.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS _migrate_lock (x INTEGER)"); err != nil {
		return fmt.Errorf("migrate: lock: %w", err)
	}

	// Read version INSIDE transaction to prevent TOCTOU
	var currentVersion int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&currentVersion); err != nil {
		return fmt.Errorf("migrate: read version: %w", err)
	}

	if currentVersion >= currentSchemaVersion {
		tx.Rollback() // explicit rollback — no work to commit
		return nil    // already migrated
	}

	// v5 → v6: add new columns for configurable phase chains, token tracking, artifact hashing, budget
	if currentVersion >= 5 && currentVersion < 6 {
		v6Stmts := []string{
			"ALTER TABLE runs ADD COLUMN phases TEXT",
			"ALTER TABLE runs ADD COLUMN token_budget INTEGER",
			"ALTER TABLE runs ADD COLUMN budget_warn_pct INTEGER DEFAULT 80",
			"ALTER TABLE dispatches ADD COLUMN cache_hits INTEGER",
			"ALTER TABLE run_artifacts ADD COLUMN content_hash TEXT",
			"ALTER TABLE run_artifacts ADD COLUMN dispatch_id TEXT",
		}
		for _, stmt := range v6Stmts {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				// Column may already exist from a partial prior run — ignore "duplicate column" errors
				if !isDuplicateColumnError(err) {
					return fmt.Errorf("migrate v5→v6: %w", err)
				}
			}
		}
	}

	// v4–v7 → v8: add status column to run_artifacts
	// Guard: run_artifacts exists from v4+. For v0-v3, the DDL creates it with the column already.
	if currentVersion >= 4 && currentVersion < 8 {
		v8Stmts := []string{
			"ALTER TABLE run_artifacts ADD COLUMN status TEXT NOT NULL DEFAULT 'active'",
		}
		for _, stmt := range v8Stmts {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				if !isDuplicateColumnError(err) {
					return fmt.Errorf("migrate v7→v8: %w", err)
				}
			}
		}
	}

	// v3–v9 → v10: portfolio orchestration columns
	// Guard: runs table exists from v3+. For v0-v2, the DDL creates it with the columns already.
	if currentVersion >= 3 && currentVersion < 10 {
		v10Stmts := []string{
			"ALTER TABLE runs ADD COLUMN parent_run_id TEXT",
			"ALTER TABLE runs ADD COLUMN max_dispatches INTEGER DEFAULT 0",
		}
		for _, stmt := range v10Stmts {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				if !isDuplicateColumnError(err) {
					return fmt.Errorf("migrate v9→v10: %w", err)
				}
			}
		}
	}

	// v2–v10 → v11: TOCTOU conflict detection columns + merge_intents table
	// Guard: dispatches table exists from v2+. For v0-v1, the DDL creates it with the columns already.
	if currentVersion >= 2 && currentVersion < 11 {
		v11Stmts := []string{
			"ALTER TABLE dispatches ADD COLUMN base_repo_commit TEXT",
			"ALTER TABLE dispatches ADD COLUMN retry_count INTEGER NOT NULL DEFAULT 0",
			"ALTER TABLE dispatches ADD COLUMN conflict_type TEXT",
			"ALTER TABLE dispatches ADD COLUMN quarantine_reason TEXT",
		}
		for _, stmt := range v11Stmts {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				if !isDuplicateColumnError(err) {
					return fmt.Errorf("migrate v10→v11: %w", err)
				}
			}
		}
	}

	// v11 → v12: cost-aware scheduling columns
	// Guard: runs exists from v3+, dispatches from v2+
	if currentVersion >= 3 && currentVersion < 12 {
		v12RunStmts := []string{
			"ALTER TABLE runs ADD COLUMN budget_enforce INTEGER DEFAULT 0",
			"ALTER TABLE runs ADD COLUMN max_agents INTEGER DEFAULT 0",
		}
		for _, stmt := range v12RunStmts {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				if !isDuplicateColumnError(err) {
					return fmt.Errorf("migrate v11→v12: %w", err)
				}
			}
		}
	}
	if currentVersion >= 2 && currentVersion < 12 {
		v12DispatchStmts := []string{
			"ALTER TABLE dispatches ADD COLUMN spawn_depth INTEGER NOT NULL DEFAULT 0",
			"ALTER TABLE dispatches ADD COLUMN parent_dispatch_id TEXT NOT NULL DEFAULT ''",
		}
		for _, stmt := range v12DispatchStmts {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				if !isDuplicateColumnError(err) {
					return fmt.Errorf("migrate v11→v12: %w", err)
				}
			}
		}
	}

	// v17 → v18: sandbox specification columns
	if currentVersion >= 2 && currentVersion < 18 {
		v18Stmts := []string{
			"ALTER TABLE dispatches ADD COLUMN sandbox_spec TEXT",
			"ALTER TABLE dispatches ADD COLUMN sandbox_effective TEXT",
		}
		for _, stmt := range v18Stmts {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				if !isDuplicateColumnError(err) {
					return fmt.Errorf("migrate v17→v18: %w", err)
				}
			}
		}
	}

	// v3–v15 → v16: runtime-configurable gate rules
	if currentVersion >= 3 && currentVersion < 16 {
		v16Stmts := []string{
			"ALTER TABLE runs ADD COLUMN gate_rules TEXT",
		}
		for _, stmt := range v16Stmts {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				if !isDuplicateColumnError(err) {
					return fmt.Errorf("migrate v15→v16: %w", err)
				}
			}
		}
	}

	// v19 → v20: coordination locks + events
	if currentVersion >= 19 && currentVersion < 20 {
		if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS coordination_locks (
			id TEXT PRIMARY KEY,
			type TEXT NOT NULL CHECK(type IN ('file_reservation','named_lock','write_set')),
			owner TEXT NOT NULL,
			scope TEXT NOT NULL,
			pattern TEXT NOT NULL,
			exclusive INTEGER NOT NULL DEFAULT 1,
			reason TEXT,
			ttl_seconds INTEGER,
			created_at INTEGER NOT NULL,
			expires_at INTEGER,
			released_at INTEGER,
			dispatch_id TEXT,
			run_id TEXT)`); err != nil {
			return fmt.Errorf("v20 coordination_locks: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS coordination_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			lock_id TEXT NOT NULL,
			run_id TEXT,
			event_type TEXT NOT NULL,
			owner TEXT NOT NULL,
			pattern TEXT NOT NULL,
			scope TEXT NOT NULL,
			reason TEXT,
			created_at INTEGER NOT NULL DEFAULT (unixepoch()))`); err != nil {
			return fmt.Errorf("v20 coordination_events: %w", err)
		}
	}

	// v20 → v21: event envelope metadata columns (provenance/capability/trace)
	if currentVersion >= 3 && currentVersion < 21 {
		if _, err := tx.ExecContext(ctx, "ALTER TABLE phase_events ADD COLUMN envelope_json TEXT"); err != nil {
			if !isDuplicateColumnError(err) {
				return fmt.Errorf("migrate v20→v21 phase_events: %w", err)
			}
		}
	}
	if currentVersion >= 5 && currentVersion < 21 {
		if _, err := tx.ExecContext(ctx, "ALTER TABLE dispatch_events ADD COLUMN envelope_json TEXT"); err != nil {
			if !isDuplicateColumnError(err) {
				return fmt.Errorf("migrate v20→v21 dispatch_events: %w", err)
			}
		}
	}
	if currentVersion >= 19 && currentVersion < 21 {
		if _, err := tx.ExecContext(ctx, "ALTER TABLE coordination_events ADD COLUMN envelope_json TEXT"); err != nil {
			if !isDuplicateColumnError(err) {
				return fmt.Errorf("migrate v20→v21 coordination_events: %w", err)
			}
		}
	}

	// v21 → v22: replay input capture table for deterministic run replay
	if currentVersion >= 3 && currentVersion < 22 {
		if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS run_replay_inputs (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			run_id       TEXT NOT NULL REFERENCES runs(id),
			kind         TEXT NOT NULL,
			input_key    TEXT,
			payload      TEXT NOT NULL DEFAULT '{}',
			artifact_ref TEXT,
			event_source TEXT,
			event_id     INTEGER,
			created_at   INTEGER NOT NULL DEFAULT (unixepoch())
		)`); err != nil {
			return fmt.Errorf("migrate v21→v22 run_replay_inputs: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_replay_inputs_run_created
			ON run_replay_inputs(run_id, created_at, id)`); err != nil {
			return fmt.Errorf("migrate v21→v22 idx_replay_inputs_run_created: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_replay_inputs_run_kind
			ON run_replay_inputs(run_id, kind, created_at, id)`); err != nil {
			return fmt.Errorf("migrate v21→v22 idx_replay_inputs_run_kind: %w", err)
		}
	}

	// v22 → v23: add trace_id column to audit_log for trace correlation
	if currentVersion >= 15 && currentVersion < 23 {
		if _, err := tx.ExecContext(ctx, "ALTER TABLE audit_log ADD COLUMN trace_id TEXT NOT NULL DEFAULT ''"); err != nil {
			if !isDuplicateColumnError(err) {
				return fmt.Errorf("migrate v22→v23 audit_log trace_id: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, "CREATE INDEX IF NOT EXISTS idx_audit_log_trace ON audit_log(trace_id) WHERE trace_id != ''"); err != nil {
			return fmt.Errorf("migrate v22→v23 idx_audit_log_trace: %w", err)
		}
	}

	// Apply schema DDL
	if _, err := tx.ExecContext(ctx, schemaDDL); err != nil {
		return fmt.Errorf("migrate: apply schema: %w", err)
	}

	// Set user_version inside same transaction
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", currentSchemaVersion)); err != nil {
		return fmt.Errorf("migrate: set version: %w", err)
	}

	return tx.Commit()
}

// Health checks that the DB is readable, schema is current, and disk has space.
func (d *DB) Health(ctx context.Context) error {
	// Check DB is readable
	var result int
	if err := d.db.QueryRowContext(ctx, "SELECT 1").Scan(&result); err != nil {
		return fmt.Errorf("health: DB not readable: %w", err)
	}

	// Check schema version
	version, err := d.SchemaVersion()
	if err != nil {
		return fmt.Errorf("health: cannot read schema version: %w", err)
	}
	if version == 0 {
		return ErrNotMigrated
	}
	if version > maxSchemaVersion {
		return ErrSchemaVersionTooNew
	}

	// Check disk space (>10MB free)
	dir := filepath.Dir(d.path)
	if err := checkDiskSpace(dir, 10*1024*1024); err != nil {
		return fmt.Errorf("health: %w", err)
	}

	return nil
}

// isDuplicateColumnError checks if an ALTER TABLE ADD COLUMN error is a "duplicate column name" error.
// This makes the v5→v6 migration idempotent: if a prior run added some columns before failing,
// the retry will skip those columns instead of failing permanently.
func isDuplicateColumnError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "duplicate column name")
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}
