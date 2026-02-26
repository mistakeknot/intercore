package replay

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/mistakeknot/intercore/internal/db"
)

func setupReplayStore(t *testing.T) (*Store, *db.DB) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	d, err := db.Open(path, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	if err := d.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return New(d.SqlDB()), d
}

func insertReplayRun(t *testing.T, d *db.DB, runID string) {
	t.Helper()
	_, err := d.SqlDB().ExecContext(context.Background(), `
		INSERT INTO runs (id, project_dir, goal, status, phase, complexity, force_full, auto_advance, created_at, updated_at)
		VALUES (?, '/tmp', 'replay test', 'completed', 'done', 3, 0, 1, unixepoch(), unixepoch())`, runID)
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
}

func TestAddAndListInputs(t *testing.T) {
	store, d := setupReplayStore(t)
	ctx := context.Background()
	insertReplayRun(t, d, "run-replay-1")

	eventID := int64(42)
	artifact := "phase_event:42"
	if _, err := store.AddInput(ctx, &Input{
		RunID:       "run-replay-1",
		Kind:        "time",
		Key:         "phase_transition",
		Payload:     `{"from":"a","to":"b"}`,
		ArtifactRef: &artifact,
		EventSource: "phase",
		EventID:     &eventID,
	}); err != nil {
		t.Fatalf("AddInput #1: %v", err)
	}
	if _, err := store.AddInput(ctx, &Input{
		RunID:   "run-replay-1",
		Kind:    "external",
		Key:     "dispatch_transition",
		Payload: `{"from":"spawned","to":"running"}`,
	}); err != nil {
		t.Fatalf("AddInput #2: %v", err)
	}

	items, err := store.ListInputs(ctx, "run-replay-1", 10)
	if err != nil {
		t.Fatalf("ListInputs: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("ListInputs len=%d, want 2", len(items))
	}
	if items[0].RunID != "run-replay-1" {
		t.Fatalf("RunID=%q, want run-replay-1", items[0].RunID)
	}
	if items[0].EventID == nil || *items[0].EventID != 42 {
		t.Fatalf("EventID=%v, want 42", items[0].EventID)
	}
	if items[0].ArtifactRef == nil || *items[0].ArtifactRef != artifact {
		t.Fatalf("ArtifactRef=%v, want %q", items[0].ArtifactRef, artifact)
	}
}

func TestAddInputValidation(t *testing.T) {
	store, _ := setupReplayStore(t)
	ctx := context.Background()

	if _, err := store.AddInput(ctx, nil); err == nil {
		t.Fatal("expected nil input error")
	}
	if _, err := store.AddInput(ctx, &Input{Kind: "time"}); err == nil {
		t.Fatal("expected run_id required error")
	}
	if _, err := store.AddInput(ctx, &Input{RunID: "r1"}); err == nil {
		t.Fatal("expected kind required error")
	}
}
