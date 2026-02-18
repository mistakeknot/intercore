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
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaDDL string

const (
	currentSchemaVersion = 1
	maxSchemaVersion     = 1
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
