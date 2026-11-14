package db

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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
	keep := 5
	if value := os.Getenv("IC_BACKUP_KEEP"); value != "" {
		keep, err = strconv.Atoi(value)
		if err != nil || keep < 1 {
			return fmt.Errorf("migrate: IC_BACKUP_KEEP must be a positive integer")
		}
	}
	dir := os.Getenv("IC_BACKUP_DIR")
	if dir == "" {
		dir = filepath.Dir(d.path)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("migrate: backup directory: %w", err)
	}
	need := uint64(info.Size())
	if wal, err := os.Stat(d.path + "-wal"); err == nil {
		need += uint64(wal.Size())
	}
	// Room for the snapshot plus the 10 MiB Health floor.
	if err := diskSpaceCheck(dir, need+10*1024*1024); err != nil {
		return fmt.Errorf("migrate: backup: not enough free space: %w", err)
	}

	// Publish only verified snapshots. Pending names do not match retention,
	// so another process cannot prune a snapshot while it is being written.
	f, err := os.CreateTemp(dir, ".intercore-backup-pending-*")
	if err != nil {
		return fmt.Errorf("migrate: backup failed: %w", err)
	}
	pending := f.Name()
	f.Close()
	defer os.Remove(pending)
	if err := migrateBackupFn(ctx, d, pending); err != nil {
		return fmt.Errorf("migrate: backup failed: %w", err)
	}
	if err := verifyBackup(ctx, pending, fromVersion); err != nil {
		return fmt.Errorf("migrate: backup failed: %w", err)
	}
	// Linking publishes atomically without replacing any same-second backup.
	stamp := time.Now().Format("20060102-150405")
	base := filepath.Join(dir, filepath.Base(d.path))
	var target string
	for n := 0; ; n++ {
		target = fmt.Sprintf("%s.backup-%d-%s.db", base, fromVersion, stamp)
		if n > 0 {
			target = fmt.Sprintf("%s.backup-%d-%s-%d.db", base, fromVersion, stamp, n)
		}
		err := os.Link(pending, target)
		if err == nil {
			break
		}
		if !os.IsExist(err) {
			return fmt.Errorf("migrate: backup failed: reserve %s: %w", target, err)
		}
	}

	return pruneBackups(dir, filepath.Base(d.path), target, keep)
}

// Match only this database's two historical snapshot formats. Do not follow
// symlinks or remove directories, pending files, or similarly named files.
func pruneBackups(dir, base, newest string, keep int) error {
	pattern := regexp.MustCompile(`^` + regexp.QuoteMeta(base) + `\.backup-(?:[0-9]{8}-[0-9]{6}|[0-9]+-[0-9]{8}-[0-9]{6}(?:-[0-9]+)?\.db)$`)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("migrate: backup retention: %w", err)
	}
	type snapshot struct {
		path     string
		modified time.Time
	}
	var snapshots []snapshot
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		if path == newest || !pattern.MatchString(entry.Name()) || !entry.Type().IsRegular() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("migrate: backup retention: %w", err)
		}
		snapshots = append(snapshots, snapshot{path, info.ModTime()})
	}
	sort.Slice(snapshots, func(i, j int) bool {
		if snapshots[i].modified.Equal(snapshots[j].modified) {
			return snapshots[i].path > snapshots[j].path
		}
		return snapshots[i].modified.After(snapshots[j].modified)
	})
	// The newly verified snapshot always occupies one retention slot.
	for i := keep - 1; i < len(snapshots); i++ {
		if err := os.Remove(snapshots[i].path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("migrate: backup retention: %w", err)
		}
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
