package landed

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/mistakeknot/intercore/internal/db"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	store, _ := testStoreWithDB(t)
	return store
}

// testStoreWithDB returns the store plus the underlying *sql.DB so tests can
// seed referenced tables (dispatches, runs, sessions) directly.
func testStoreWithDB(t *testing.T) (*Store, *sql.DB) {
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
	return NewStore(d.SqlDB()), d.SqlDB()
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

// seedDispatch inserts a minimal valid dispatches row (id is the TEXT PK).
func seedDispatch(t *testing.T, sqlDB *sql.DB, id string) {
	t.Helper()
	_, err := sqlDB.ExecContext(context.Background(),
		`INSERT INTO dispatches (id, project_dir) VALUES (?, ?)`, id, "/project")
	if err != nil {
		t.Fatalf("seed dispatch %q: %v", id, err)
	}
}

// seedRun inserts a minimal valid runs row (id is the TEXT PK).
func seedRun(t *testing.T, sqlDB *sql.DB, id string) {
	t.Helper()
	_, err := sqlDB.ExecContext(context.Background(),
		`INSERT INTO runs (id, project_dir, goal) VALUES (?, ?, ?)`, id, "/project", "goal")
	if err != nil {
		t.Fatalf("seed run %q: %v", id, err)
	}
}

// seedSession inserts a minimal valid sessions row (session_id is a column,
// unique only together with project_dir).
func seedSession(t *testing.T, sqlDB *sql.DB, sessionID string) {
	t.Helper()
	_, err := sqlDB.ExecContext(context.Background(),
		`INSERT INTO sessions (session_id, project_dir) VALUES (?, ?)`, sessionID, "/project")
	if err != nil {
		t.Fatalf("seed session %q: %v", sessionID, err)
	}
}

// TestAudit_CleanRefs: a landed row whose dispatch/run/session all reference
// existing rows produces zero orphans.
func TestAudit_CleanRefs(t *testing.T) {
	store, sqlDB := testStoreWithDB(t)
	ctx := context.Background()

	seedDispatch(t, sqlDB, "dispatch-ok")
	seedRun(t, sqlDB, "run-ok")
	seedSession(t, sqlDB, "session-ok")

	if _, err := store.Record(ctx, RecordOpts{
		CommitSHA:  "clean-commit",
		ProjectDir: "/project",
		DispatchID: "dispatch-ok",
		RunID:      "run-ok",
		SessionID:  "session-ok",
		BeadID:     "iv-bead-ok",
	}); err != nil {
		t.Fatalf("Record clean: %v", err)
	}

	res, err := store.Audit(ctx)
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}

	if res.DispatchOrphans != 0 {
		t.Errorf("DispatchOrphans = %d, want 0", res.DispatchOrphans)
	}
	if res.RunOrphans != 0 {
		t.Errorf("RunOrphans = %d, want 0", res.RunOrphans)
	}
	if res.SessionOrphans != 0 {
		t.Errorf("SessionOrphans = %d, want 0", res.SessionOrphans)
	}
	if res.TotalOrphans != 0 {
		t.Errorf("TotalOrphans = %d, want 0", res.TotalOrphans)
	}
	// bead_id is structurally present but not auditable against a local table.
	if res.BeadRefsPresent != 1 {
		t.Errorf("BeadRefsPresent = %d, want 1", res.BeadRefsPresent)
	}
	if res.BeadAudited {
		t.Error("BeadAudited = true, want false (no local beads table)")
	}
}

// TestAudit_BogusRefs: landed rows with bogus dispatch_id/run_id/session_id are
// counted as orphans; a clean row alongside them is not.
func TestAudit_BogusRefs(t *testing.T) {
	store, sqlDB := testStoreWithDB(t)
	ctx := context.Background()

	// One fully-valid reference set.
	seedDispatch(t, sqlDB, "dispatch-ok")
	seedRun(t, sqlDB, "run-ok")
	seedSession(t, sqlDB, "session-ok")

	// Clean row — references the seeded rows.
	if _, err := store.Record(ctx, RecordOpts{
		CommitSHA:  "clean-commit",
		ProjectDir: "/project",
		DispatchID: "dispatch-ok",
		RunID:      "run-ok",
		SessionID:  "session-ok",
	}); err != nil {
		t.Fatalf("Record clean: %v", err)
	}

	// Bogus dispatch_id (run/session valid) — 1 dispatch orphan.
	if _, err := store.Record(ctx, RecordOpts{
		CommitSHA:  "bad-dispatch",
		ProjectDir: "/project",
		DispatchID: "dispatch-MISSING",
		RunID:      "run-ok",
		SessionID:  "session-ok",
	}); err != nil {
		t.Fatalf("Record bad-dispatch: %v", err)
	}

	// Bogus run_id (dispatch/session valid) — 1 run orphan.
	if _, err := store.Record(ctx, RecordOpts{
		CommitSHA:  "bad-run",
		ProjectDir: "/project",
		DispatchID: "dispatch-ok",
		RunID:      "run-MISSING",
		SessionID:  "session-ok",
	}); err != nil {
		t.Fatalf("Record bad-run: %v", err)
	}

	// Bogus session_id (dispatch/run valid) — 1 session orphan.
	if _, err := store.Record(ctx, RecordOpts{
		CommitSHA:  "bad-session",
		ProjectDir: "/project",
		DispatchID: "dispatch-ok",
		RunID:      "run-ok",
		SessionID:  "session-MISSING",
	}); err != nil {
		t.Fatalf("Record bad-session: %v", err)
	}

	// All-bogus row — counts once in each of the three columns.
	if _, err := store.Record(ctx, RecordOpts{
		CommitSHA:  "all-bad",
		ProjectDir: "/project",
		DispatchID: "dispatch-XXX",
		RunID:      "run-XXX",
		SessionID:  "session-XXX",
	}); err != nil {
		t.Fatalf("Record all-bad: %v", err)
	}

	res, err := store.Audit(ctx)
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}

	// bad-dispatch + all-bad = 2 dispatch orphans.
	if res.DispatchOrphans != 2 {
		t.Errorf("DispatchOrphans = %d, want 2", res.DispatchOrphans)
	}
	// bad-run + all-bad = 2 run orphans.
	if res.RunOrphans != 2 {
		t.Errorf("RunOrphans = %d, want 2", res.RunOrphans)
	}
	// bad-session + all-bad = 2 session orphans.
	if res.SessionOrphans != 2 {
		t.Errorf("SessionOrphans = %d, want 2", res.SessionOrphans)
	}
	if res.TotalOrphans != 6 {
		t.Errorf("TotalOrphans = %d, want 6", res.TotalOrphans)
	}
}

// TestAudit_NullRefsNotOrphans: NULL FK columns are never orphans (only
// non-NULL ids with no referent count).
func TestAudit_NullRefsNotOrphans(t *testing.T) {
	store, _ := testStoreWithDB(t)
	ctx := context.Background()

	// No dispatch/run/session ids at all — all NULL after NULLIF(?, '').
	if _, err := store.Record(ctx, RecordOpts{
		CommitSHA:  "null-refs",
		ProjectDir: "/project",
	}); err != nil {
		t.Fatalf("Record null-refs: %v", err)
	}

	res, err := store.Audit(ctx)
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if res.TotalOrphans != 0 {
		t.Errorf("TotalOrphans = %d, want 0 (NULL refs are not orphans)", res.TotalOrphans)
	}
	if res.BeadRefsPresent != 0 {
		t.Errorf("BeadRefsPresent = %d, want 0", res.BeadRefsPresent)
	}
}

// TestAudit_EmptyTable: auditing an empty table returns all-zero counts.
func TestAudit_EmptyTable(t *testing.T) {
	store := testStore(t)
	res, err := store.Audit(context.Background())
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if res.TotalOrphans != 0 || res.BeadRefsPresent != 0 {
		t.Errorf("empty audit = %+v, want all zero", res)
	}
}
