package db

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A '#' or '?' in the database path must not truncate the DSN.
//
// Open builds a `file:` URI, which SQLite parses as a URI: an unescaped '#'
// begins the fragment and an unescaped '?' begins the query, so everything
// after it -- the rest of the path, the filename, every _pragma -- is thrown
// away and SQLite opens whatever the truncated prefix names. It reports no
// error while doing it.
//
// This is how TestSpawn_ForwardsRequestedBackend failed for weeks. `go test`
// names the subtest of an empty string "#00" and embeds that in t.TempDir(),
// so the test asked for
//
//	$TMPDIR/TestSpawn_ForwardsRequestedBackend#004034479485/001/test.db
//
// and got
//
//	$TMPDIR/TestSpawn_ForwardsRequestedBackend
//
// -- one file in the shared temp ROOT, reused by every run on the machine. It
// accumulated 41 tables and reached user_version 40 under a binary that
// supported 40, and every later run against a 39-binary died with
// ErrSchemaVersionTooNew against a database it had never meant to open.
//
// The assertion is deliberately about WHICH FILE EXISTS afterwards rather than
// about the absence of an error: the failure mode is silent success on the
// wrong file, so a test that only checked err would have passed throughout.
func TestOpenPathWithURIMetacharacters(t *testing.T) {
	for _, name := range []string{"plain", "with#hash", "with?question", "relative"} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), name)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "test.db")

			// A RELATIVE path has to keep working: `ic init` opens
			// "intercore.db" in the project directory. Escaping the path with
			// url.URL.String() instead of EscapedPath() inserts "//" after the
			// scheme and SQLite reads the filename as a URI authority --
			// "invalid uri authority: intercore.db", which broke every cmd/ic
			// test the first time this was fixed.
			if name == "relative" {
				cwd, err := os.Getwd()
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Chdir(dir); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chdir(cwd) })
				path = "test.db"
			}

			d, err := Open(path, 100*time.Millisecond)
			if err != nil {
				t.Fatalf("Open(%q): %v", path, err)
			}
			defer d.Close()
			if _, err := d.SqlDB().Exec("CREATE TABLE probe (x INTEGER)"); err != nil {
				t.Fatalf("write to %q: %v", path, err)
			}

			if _, err := os.Stat(path); err != nil {
				t.Fatalf("Open(%q) reported success but that file does not exist: %v\n"+
					"the DSN was truncated and some other database was opened", path, err)
			}
		})
	}
}
