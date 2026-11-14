// Package testutil provides isolation checks for tests that invoke ic.
package testutil

import (
	"os"
	"path/filepath"
	"testing"
)

// FailOnAncestorDB prevents ic's working-directory discovery from opening a
// database outside a temporary test root. Call before running any ic commands.
func FailOnAncestorDB(t testing.TB, root string) {
	t.Helper()
	dir := root
	for {
		p := filepath.Join(dir, ".clavain", "intercore.db")
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("database %s exists above the temp root %s; working-directory discovery could write to it. Run with TMPDIR set to a directory outside that tree.", p, root)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return
		}
		dir = parent
	}
}
