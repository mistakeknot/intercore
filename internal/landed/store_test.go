package landed

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/mistakeknot/intercore/internal/db"
)

func testStore(t *testing.T) *Store {
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
	return NewStore(d.SqlDB())
}

func TestRecord(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	id, err := store.Record(ctx, RecordOpts{
		CommitSHA:  "abc123def456",
		ProjectDir: "/home/user/project",
		Branch:     "main",
		DispatchID: "dispatch-1",
		RunID:      "run-1",
		BeadID:     "iv-test1",
		SessionID:  "session-1",
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if id <= 0 {
		t.Errorf("Record returned id=%d, want > 0", id)
	}
}

func TestRecord_Idempotent(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	opts := RecordOpts{
		CommitSHA:  "abc123def456",
		ProjectDir: "/home/user/project",
	}

	id1, err := store.Record(ctx, opts)
	if err != nil {
		t.Fatalf("Record 1: %v", err)
	}

	id2, err := store.Record(ctx, opts)
	if err != nil {
		t.Fatalf("Record 2: %v", err)
	}

	if id1 != id2 {
		t.Errorf("Idempotent record: id1=%d, id2=%d (should match)", id1, id2)
	}
}

func TestRecord_DefaultBranch(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	store.Record(ctx, RecordOpts{
		CommitSHA:  "abc123",
		ProjectDir: "/project",
	})

	changes, err := store.List(ctx, ListOpts{ProjectDir: "/project"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("expected 1 change, got %d", len(changes))
	}
	if changes[0].Branch != "main" {
		t.Errorf("Branch = %q, want %q", changes[0].Branch, "main")
	}
}

func TestMarkReverted(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	store.Record(ctx, RecordOpts{
		CommitSHA:  "abc123",
		ProjectDir: "/project",
	})

	if err := store.MarkReverted(ctx, "abc123", "/project", "revert123"); err != nil {
		t.Fatalf("MarkReverted: %v", err)
	}

	// Should not appear in default list (excludes reverted)
	changes, err := store.List(ctx, ListOpts{ProjectDir: "/project"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("expected 0 non-reverted changes, got %d", len(changes))
	}

	// Should appear with IncludeReverted
	changes, err = store.List(ctx, ListOpts{ProjectDir: "/project", IncludeReverted: true})
	if err != nil {
		t.Fatalf("List with reverted: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("expected 1 change, got %d", len(changes))
	}
	if changes[0].RevertedAt == nil {
		t.Error("expected RevertedAt to be set")
	}
	if changes[0].RevertedBy == nil || *changes[0].RevertedBy != "revert123" {
		t.Errorf("RevertedBy = %v, want %q", changes[0].RevertedBy, "revert123")
	}
}

func TestMarkReverted_NotFound(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	err := store.MarkReverted(ctx, "nonexistent", "/project", "revert123")
	if err == nil {
		t.Error("expected error for non-existent commit")
	}
}

func TestList_Filters(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	store.Record(ctx, RecordOpts{CommitSHA: "c1", ProjectDir: "/p1", BeadID: "bead-1"})
	store.Record(ctx, RecordOpts{CommitSHA: "c2", ProjectDir: "/p1", BeadID: "bead-2"})
	store.Record(ctx, RecordOpts{CommitSHA: "c3", ProjectDir: "/p2", BeadID: "bead-1"})

	// Filter by project
	changes, _ := store.List(ctx, ListOpts{ProjectDir: "/p1"})
	if len(changes) != 2 {
		t.Errorf("project filter: got %d, want 2", len(changes))
	}

	// Filter by bead
	changes, _ = store.List(ctx, ListOpts{BeadID: "bead-1"})
	if len(changes) != 2 {
		t.Errorf("bead filter: got %d, want 2", len(changes))
	}

	// Filter by both
	changes, _ = store.List(ctx, ListOpts{ProjectDir: "/p1", BeadID: "bead-1"})
	if len(changes) != 1 {
		t.Errorf("project+bead filter: got %d, want 1", len(changes))
	}
}

func TestSummary(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	store.Record(ctx, RecordOpts{CommitSHA: "c1", ProjectDir: "/p", BeadID: "b1", RunID: "r1"})
	store.Record(ctx, RecordOpts{CommitSHA: "c2", ProjectDir: "/p", BeadID: "b1", RunID: "r1"})
	store.Record(ctx, RecordOpts{CommitSHA: "c3", ProjectDir: "/p", BeadID: "b2", RunID: "r2"})

	// Revert one
	store.MarkReverted(ctx, "c3", "/p", "revert-c3")

	summary, err := store.Summary(ctx, ListOpts{ProjectDir: "/p"})
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}

	if summary.Total != 3 {
		t.Errorf("Total = %d, want 3", summary.Total)
	}
	if summary.Reverted != 1 {
		t.Errorf("Reverted = %d, want 1", summary.Reverted)
	}
	if summary.ByBead["b1"] != 2 {
		t.Errorf("ByBead[b1] = %d, want 2", summary.ByBead["b1"])
	}
	// b2's only commit was reverted, so it shouldn't appear in ByBead (which excludes reverted)
	if summary.ByBead["b2"] != 0 {
		t.Errorf("ByBead[b2] = %d, want 0 (reverted)", summary.ByBead["b2"])
	}
}
