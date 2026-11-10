package db

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// OpenExisting must fail on a missing file and create nothing, where Open
// creates it; the pair is the control.
func TestOpenExistingDoesNotCreate(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.db")
	if d, err := OpenExisting(missing, time.Second); err == nil {
		d.Close()
		t.Fatal("OpenExisting opened a missing file")
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if _, err := os.Stat(missing + suffix); err == nil {
			t.Fatalf("OpenExisting created %s", missing+suffix)
		}
	}

	made := filepath.Join(dir, "made.db")
	d, err := Open(made, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	d.Close() // control: Open does create the file
	if _, err := os.Stat(made); err != nil {
		t.Fatalf("control failed: Open did not create the file: %v", err)
	}
	e, err := OpenExisting(made, time.Second)
	if err != nil {
		t.Fatalf("OpenExisting on an existing file: %v", err)
	}
	e.Close()
}

// OpenExisting changes no persistent state: a rollback-journal database stays
// in rollback mode until EnableWAL is called, so a runner that opened the wrong
// file has not altered it.
func TestOpenExistingKeepsJournalModeUntilEnableWAL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q.db")
	d, err := Open(path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.SqlDB().Exec("PRAGMA journal_mode = DELETE"); err != nil {
		t.Fatal(err)
	}
	d.Close()

	mode := func(db *DB) string {
		var m string
		if err := db.SqlDB().QueryRow("PRAGMA journal_mode").Scan(&m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	e, err := OpenExisting(path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got := mode(e); got != "delete" {
		t.Fatalf("OpenExisting changed the journal mode to %q", got)
	}
	if err := e.EnableWAL(); err != nil {
		t.Fatal(err)
	}
	if got := mode(e); got != "wal" {
		t.Fatalf("after EnableWAL: %q", got)
	}
	e.Close()
}

// OpenExisting reads nothing, so it does not check the schema version;
// CheckSchemaVersion does, after the caller has verified the file. Open still
// checks at once (the control).
func TestOpenExistingDefersSchemaVersionCheck(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new.db")
	d, err := Open(path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.SqlDB().Exec("PRAGMA user_version = 9999"); err != nil {
		t.Fatal(err)
	}
	d.Close()

	if d2, err := Open(path, time.Second); err != ErrSchemaVersionTooNew {
		if d2 != nil {
			d2.Close()
		}
		t.Fatalf("Open: got %v, want ErrSchemaVersionTooNew", err)
	}
	e, err := OpenExisting(path, time.Second)
	if err != nil {
		t.Fatalf("OpenExisting read the version before the caller verified the file: %v", err)
	}
	defer e.Close()
	if err := e.CheckSchemaVersion(); err != ErrSchemaVersionTooNew {
		t.Fatalf("CheckSchemaVersion: got %v", err)
	}
}
