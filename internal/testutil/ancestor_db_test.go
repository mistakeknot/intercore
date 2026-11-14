package testutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestFailOnAncestorDB(t *testing.T) {
	// Run the fatal guard in a subprocess so the parent can assert its failure.
	if root := os.Getenv("IC_TEST_ANCESTOR_DB_ROOT"); root != "" {
		FailOnAncestorDB(t, root)
		return
	}
	for _, tc := range []struct {
		name string
		rel  string
	}{
		{"clean", ""},
		{"root", "a/b"},
		{"parent", "a"},
		{"distant_ancestor", "."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "a", "b")
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}
			var dbPath string
			if tc.rel != "" {
				dbPath = filepath.Join(base, tc.rel, ".clavain", "intercore.db")
				if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
					t.Fatal(err)
				}
				// A dummy marker is sufficient: the guard must never open the DB.
				if err := os.WriteFile(dbPath, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestFailOnAncestorDB$")
			cmd.Env = append(os.Environ(), "IC_TEST_ANCESTOR_DB_ROOT="+root)
			out, err := cmd.CombinedOutput()
			if tc.rel == "" {
				if err != nil {
					t.Fatalf("clean tree rejected: %v\n%s", err, out)
				}
				return
			}
			if err == nil {
				t.Fatal("guard accepted an ancestor database")
			}
			if !strings.Contains(string(out), dbPath) || !strings.Contains(string(out), "TMPDIR") {
				t.Fatalf("missing database path or remediation in guard failure:\n%s", out)
			}
			if info, err := os.Stat(dbPath); err != nil || info.Size() != 0 {
				t.Fatalf("dummy database changed: %v", err)
			}
		})
	}
}
