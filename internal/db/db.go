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
	currentSchemaVersion = 8
	maxSchemaVersion     = 8
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
		busyTimeout = 100 * time.Millisecond
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
		return nil // already migrated
	}

	// v5 → v6: add new columns for configurable phase chains, token tracking, artifact hashing, budget
	if currentVersion >= 5 {
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

	// v7 → v8: add status column to run_artifacts
	// Guard: run_artifacts exists from v4+. For v0-v3, the DDL below creates it with the column.
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
