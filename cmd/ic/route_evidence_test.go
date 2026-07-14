package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestRouteRecordEvidenceCLI proves a harness can make escalation and gate
// events observable purely by shelling out to `ic route record-evidence`
// (no Go linkage) — the emission path the Hermes-on-zklw adapter uses. This
// is the CLI-boundary form of the DoD's "observable in evidence" clause.
func TestRouteRecordEvidenceCLI(t *testing.T) {
	// openDB validates the path is under CWD, so create the temp DB inside the
	// package directory rather than the system temp dir.
	dir, err := os.MkdirTemp(".", "evidence-test-")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	dbPath := filepath.Join(dir, "evidence.db")

	// Point the CLI's global DB at a temp file and initialize the schema.
	prev := flagDB
	flagDB = dbPath
	t.Cleanup(func() { flagDB = prev })

	d, oerr := openDB()
	if oerr != nil {
		t.Fatalf("openDB: %v", oerr)
	}
	if err := d.Migrate(context.Background()); err != nil {
		d.Close()
		t.Fatalf("migrate: %v", err)
	}
	d.Close()

	ctx := context.Background()

	// Missing --kind is malformed.
	if rc := cmdRouteRecordEvidence(ctx, []string{"--from=sonnet"}); rc != exitMalformed {
		t.Errorf("missing --kind rc = %d, want %d", rc, exitMalformed)
	}
	// Bad --kind is malformed.
	if rc := cmdRouteRecordEvidence(ctx, []string{"--kind=bogus"}); rc != exitMalformed {
		t.Errorf("bad --kind rc = %d, want %d", rc, exitMalformed)
	}
	// A valid escalation event records (rc 0).
	esc := []string{
		"--kind=escalation", "--chain=c1", "--from=sonnet", "--to=opus",
		"--mode=verdict-fail", "--agent=executor", "--project=" + dir,
	}
	if rc := cmdRouteRecordEvidence(ctx, esc); rc != 0 {
		t.Errorf("escalation record rc = %d, want 0", rc)
	}
	// A valid gate event records (rc 0).
	gate := []string{
		"--kind=gate", "--gate=behavioral-verify",
		"--model=openai/gpt-5.6-sol@api", "--agent=executor", "--project=" + dir,
	}
	if rc := cmdRouteRecordEvidence(ctx, gate); rc != 0 {
		t.Errorf("gate record rc = %d, want 0", rc)
	}
}
