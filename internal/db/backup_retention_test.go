package db

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mistakeknot/intercore/internal/testutil"
)

func backupFixture(t *testing.T) (*DB, string) {
	t.Helper()
	t.Setenv("IC_BACKUP_DIR", "")
	t.Setenv("IC_BACKUP_KEEP", "")
	d, path := tempDB(t)
	testutil.FailOnAncestorDB(t, filepath.Dir(path))
	if err := d.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Setup may snapshot the newly opened database's empty schema.
	for _, backup := range backupsOf(t, path) {
		if err := os.Remove(backup); err != nil {
			t.Fatal(err)
		}
	}
	return d, path
}

func TestBackupFailureDoesNotPrune(t *testing.T) {
	d, path := backupFixture(t)
	t.Setenv("IC_BACKUP_KEEP", "1")
	old := path + ".backup-20200101-000000"
	if err := os.WriteFile(old, []byte("dummy"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := d.backupBeforeMigrate(context.Background(), currentSchemaVersion-1); err == nil {
		t.Fatal("unverified backup accepted")
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatal("existing backup removed after failed verification")
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".intercore-backup-pending-") {
			t.Fatal("partial backup leaked")
		}
	}
}

func TestBackupRetention(t *testing.T) {
	for _, keep := range []int{5, 2} {
		t.Run(strconv.Itoa(keep), func(t *testing.T) {
			d, path := backupFixture(t)
			if keep != 5 {
				t.Setenv("IC_BACKUP_KEEP", strconv.Itoa(keep))
			}
			// Both historical naming formats participate in retention.
			var old []string
			for i := 0; i < 6; i++ {
				name := path + ".backup-20200101-00000" + strconv.Itoa(i)
				if i%2 == 1 {
					name = path + ".backup-40-20200101-00000" + strconv.Itoa(i) + ".db"
				}
				if err := os.WriteFile(name, []byte("dummy"), 0600); err != nil {
					t.Fatal(err)
				}
				stamp := time.Unix(int64(i+1), 0)
				if err := os.Chtimes(name, stamp, stamp); err != nil {
					t.Fatal(err)
				}
				old = append(old, name)
			}
			// Nearby names, directories and symlinks must survive.
			protected := []string{path + ".backup-not-a-snapshot", path + ".backup-40-20200101-000000.db.notes", path + ".backup-40-20200101-000000.db.pending", path + ".other.backup-20200101-000000"}
			for _, name := range protected {
				if err := os.WriteFile(name, []byte("dummy"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			dir := path + ".backup-20200101-000007"
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			link := path + ".backup-20200101-000008"
			if err := os.Symlink(protected[0], link); err != nil {
				t.Fatal(err)
			}
			if err := d.backupBeforeMigrate(context.Background(), currentSchemaVersion); err != nil {
				t.Fatal(err)
			}
			for i, name := range old {
				_, err := os.Stat(name)
				if want := i >= len(old)-(keep-1); want != (err == nil) {
					t.Errorf("old backup %d exists=%v, want %v", i, err == nil, want)
				}
			}
			for _, name := range append(protected, dir, link) {
				if _, err := os.Lstat(name); err != nil {
					t.Errorf("protected entry removed: %v", err)
				}
			}
		})
	}
}

func TestBackupDirectory(t *testing.T) {
	d, path := backupFixture(t)
	old := path + ".backup-20200101-000000"
	if err := os.WriteFile(old, []byte("dummy"), 0600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "backup directory")
	testutil.FailOnAncestorDB(t, dir)
	t.Setenv("IC_BACKUP_DIR", dir)
	var checkedDir string
	prev := diskSpaceCheck
	diskSpaceCheck = func(dir string, size uint64) error { checkedDir = dir; return nil }
	t.Cleanup(func() { diskSpaceCheck = prev })
	if err := d.backupBeforeMigrate(context.Background(), currentSchemaVersion); err != nil {
		t.Fatal(err)
	}
	if checkedDir != dir {
		t.Fatalf("space checked in %q, want %q", checkedDir, dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries=%d, want 1", len(entries))
	}
	if err := verifyBackup(context.Background(), filepath.Join(dir, entries[0].Name()), currentSchemaVersion); err != nil {
		t.Fatal(err)
	}
	if got := backupsOf(t, path); len(got) != 1 || got[0] != old {
		t.Fatalf("source directory backups changed")
	}
}

func TestBackupInvalidKeep(t *testing.T) {
	for _, value := range []string{"0", "-1", "bad"} {
		t.Run(value, func(t *testing.T) {
			d, path := backupFixture(t)
			t.Setenv("IC_BACKUP_KEEP", value)
			if err := d.backupBeforeMigrate(context.Background(), currentSchemaVersion); err == nil {
				t.Fatal("invalid retention accepted")
			}
			if len(backupsOf(t, path)) != 0 {
				t.Fatal("backup written with invalid retention")
			}
		})
	}
}

func TestBackupConcurrentRetention(t *testing.T) {
	d, path := backupFixture(t)
	errs := make(chan error, 8)
	for i := 0; i < cap(errs); i++ {
		go func() { errs <- d.backupBeforeMigrate(context.Background(), currentSchemaVersion) }()
	}
	for i := 0; i < cap(errs); i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	backups := backupsOf(t, path)
	if len(backups) != 5 {
		t.Fatalf("backups=%d, want 5", len(backups))
	}
	for _, path := range backups {
		if err := verifyBackup(context.Background(), path, currentSchemaVersion); err != nil {
			t.Fatal(err)
		}
	}
}
