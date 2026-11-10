package db

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Test seams: a failing backup or a free-space shortfall must leave the
// database unmigrated.
var (
	migrateBackupFn = vacuumInto
	diskSpaceCheck  = checkDiskSpace
)

// vacuumInto writes a consistent, WAL-inclusive snapshot of the database to
// target. VACUUM INTO reads through the WAL, unlike a raw file copy.
func vacuumInto(ctx context.Context, d *DB, target string) error {
	_, err := d.db.ExecContext(ctx, "VACUUM INTO '"+strings.ReplaceAll(target, "'", "''")+"'")
	return err
}

// backupBeforeMigrate writes and verifies a VACUUM INTO backup of the database
// at schema version fromVersion. It does nothing for an empty or missing file.
// On any failure it removes the partial backup and returns an error; callers
// must not migrate.
func (d *DB) backupBeforeMigrate(ctx context.Context, fromVersion int) error {
	info, err := os.Stat(d.path)
	if err != nil || info.Size() == 0 {
		return nil
	}
	need := uint64(info.Size())
	if wal, err := os.Stat(d.path + "-wal"); err == nil {
		need += uint64(wal.Size())
	}
	// Room for the snapshot plus the 10 MiB Health floor.
	if err := diskSpaceCheck(filepath.Dir(d.path), need+10*1024*1024); err != nil {
		return fmt.Errorf("migrate: backup: not enough free space: %w", err)
	}

	// Reserve the target with an exclusive create, so two migrations that start
	// in the same second can never share a file: each owns the file it made and
	// removes only that one. VACUUM INTO accepts an existing empty file.
	stamp := time.Now().Format("20060102-150405")
	var target string
	for n := 0; ; n++ {
		target = fmt.Sprintf("%s.backup-%d-%s.db", d.path, fromVersion, stamp)
		if n > 0 {
			target = fmt.Sprintf("%s.backup-%d-%s-%d.db", d.path, fromVersion, stamp, n)
		}
		f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			f.Close()
			break
		}
		if !os.IsExist(err) {
			return fmt.Errorf("migrate: backup failed: reserve %s: %w", target, err)
		}
	}

	fail := func(err error) error {
		os.Remove(target)
		return fmt.Errorf("migrate: backup failed: %w", err)
	}
	if err := migrateBackupFn(ctx, d, target); err != nil {
		return fail(err)
	}
	if err := verifyBackup(ctx, target, fromVersion); err != nil {
		return fail(err)
	}
	return nil
}

// verifyBackup opens the backup read-only and requires a clean integrity
// check and the expected schema version.
func verifyBackup(ctx context.Context, path string, wantVersion int) error {
	b, err := sql.Open("sqlite", "file:"+(&url.URL{Path: path}).EscapedPath()+"?mode=ro")
	if err != nil {
		return fmt.Errorf("verify backup: %w", err)
	}
	defer b.Close()
	var res string
	if err := b.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&res); err != nil {
		return fmt.Errorf("verify backup: integrity_check: %w", err)
	}
	if res != "ok" {
		return fmt.Errorf("verify backup: integrity_check: %s", res)
	}
	var v int
	if err := b.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return fmt.Errorf("verify backup: user_version: %w", err)
	}
	if v != wantVersion {
		return fmt.Errorf("verify backup: user_version %d, want %d", v, wantVersion)
	}
	return nil
}
