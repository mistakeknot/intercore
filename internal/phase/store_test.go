package phase

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/mistakeknot/interverse/infra/intercore/internal/db"
)

func setupTestStore(t *testing.T) *Store {
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

	return New(d.SqlDB())
}

func TestStore_CreateAndGet(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	run := &Run{
		ProjectDir:  "/tmp/test",
		Goal:        "Test goal",
		Complexity:  3,
		ForceFull:   false,
		AutoAdvance: true,
	}

	id, err := store.Create(ctx, run)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(id) != 8 {
		t.Errorf("ID length = %d, want 8", len(id))
	}

	got, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if got.ProjectDir != "/tmp/test" {
		t.Errorf("ProjectDir = %q, want %q", got.ProjectDir, "/tmp/test")
	}
	if got.Goal != "Test goal" {
		t.Errorf("Goal = %q, want %q", got.Goal, "Test goal")
	}
	if got.Status != StatusActive {
		t.Errorf("Status = %q, want %q", got.Status, StatusActive)
	}
	if got.Phase != PhaseBrainstorm {
		t.Errorf("Phase = %q, want %q", got.Phase, PhaseBrainstorm)
	}
	if got.Complexity != 3 {
		t.Errorf("Complexity = %d, want 3", got.Complexity)
	}
	if got.ForceFull {
		t.Error("ForceFull = true, want false")
	}
	if !got.AutoAdvance {
		t.Error("AutoAdvance = false, want true")
	}
	if got.CompletedAt != nil {
		t.Error("CompletedAt should be nil")
	}
}

func TestStore_CreateWithScopeID(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	scope := "test-scope"
	run := &Run{
		ProjectDir:  "/tmp/test",
		Goal:        "Scoped goal",
		Complexity:  2,
		AutoAdvance: true,
		ScopeID:     &scope,
	}

	id, err := store.Create(ctx, run)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ScopeID == nil || *got.ScopeID != scope {
		t.Errorf("ScopeID = %v, want %q", got.ScopeID, scope)
	}
}

func TestStore_Get_NotFound(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	_, err := store.Get(ctx, "nonexist")
	if err != ErrNotFound {
		t.Errorf("Get(nonexist) error = %v, want ErrNotFound", err)
	}
}

func TestStore_UpdatePhase(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	id, _ := store.Create(ctx, &Run{
		ProjectDir: "/tmp", Goal: "test", Complexity: 3, AutoAdvance: true,
	})

	// Advance brainstorm → brainstorm-reviewed
	if err := store.UpdatePhase(ctx, id, PhaseBrainstorm, PhaseBrainstormReviewed); err != nil {
		t.Fatalf("UpdatePhase: %v", err)
	}

	got, _ := store.Get(ctx, id)
	if got.Phase != PhaseBrainstormReviewed {
		t.Errorf("Phase = %q, want %q", got.Phase, PhaseBrainstormReviewed)
	}
}

func TestStore_UpdatePhase_StaleDetection(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	id, _ := store.Create(ctx, &Run{
		ProjectDir: "/tmp", Goal: "test", Complexity: 3, AutoAdvance: true,
	})

	// First advance succeeds
	if err := store.UpdatePhase(ctx, id, PhaseBrainstorm, PhaseBrainstormReviewed); err != nil {
		t.Fatalf("UpdatePhase 1: %v", err)
	}

	// Second advance with stale expected phase should fail
	err := store.UpdatePhase(ctx, id, PhaseBrainstorm, PhaseBrainstormReviewed)
	if err != ErrStalePhase {
		t.Errorf("UpdatePhase(stale) error = %v, want ErrStalePhase", err)
	}
}

func TestStore_UpdatePhase_NotFound(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	err := store.UpdatePhase(ctx, "nonexist", PhaseBrainstorm, PhaseBrainstormReviewed)
	if err != ErrNotFound {
		t.Errorf("UpdatePhase(nonexist) error = %v, want ErrNotFound", err)
	}
}

func TestStore_UpdateStatus(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	id, _ := store.Create(ctx, &Run{
		ProjectDir: "/tmp", Goal: "test", Complexity: 3, AutoAdvance: true,
	})

	if err := store.UpdateStatus(ctx, id, StatusCancelled); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}

	got, _ := store.Get(ctx, id)
	if got.Status != StatusCancelled {
		t.Errorf("Status = %q, want %q", got.Status, StatusCancelled)
	}
	if got.CompletedAt == nil {
		t.Error("CompletedAt should be set for terminal status")
	}
}

func TestStore_UpdateStatus_NotFound(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	err := store.UpdateStatus(ctx, "nonexist", StatusCancelled)
	if err != ErrNotFound {
		t.Errorf("UpdateStatus(nonexist) error = %v, want ErrNotFound", err)
	}
}

func TestStore_UpdateSettings(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	id, _ := store.Create(ctx, &Run{
		ProjectDir: "/tmp", Goal: "test", Complexity: 3, AutoAdvance: true,
	})

	newComplexity := 1
	newAuto := false
	if err := store.UpdateSettings(ctx, id, &newComplexity, &newAuto, nil); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	got, _ := store.Get(ctx, id)
	if got.Complexity != 1 {
		t.Errorf("Complexity = %d, want 1", got.Complexity)
	}
	if got.AutoAdvance {
		t.Error("AutoAdvance = true, want false")
	}
}

func TestStore_ListActive(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	// Create 2 active, 1 cancelled
	id1, _ := store.Create(ctx, &Run{ProjectDir: "/a", Goal: "one", Complexity: 3, AutoAdvance: true})
	store.Create(ctx, &Run{ProjectDir: "/b", Goal: "two", Complexity: 3, AutoAdvance: true})
	id3, _ := store.Create(ctx, &Run{ProjectDir: "/c", Goal: "three", Complexity: 3, AutoAdvance: true})
	store.UpdateStatus(ctx, id3, StatusCancelled)

	runs, err := store.ListActive(ctx)
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if len(runs) != 2 {
		t.Errorf("ListActive count = %d, want 2", len(runs))
	}

	// Should be ordered by created_at DESC
	_ = id1 // just ensure it was created
}

func TestStore_ListByScopeID(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	scope1 := "scope-a"
	scope2 := "scope-b"
	store.Create(ctx, &Run{ProjectDir: "/a", Goal: "one", Complexity: 3, AutoAdvance: true, ScopeID: &scope1})
	store.Create(ctx, &Run{ProjectDir: "/b", Goal: "two", Complexity: 3, AutoAdvance: true, ScopeID: &scope2})

	runs, err := store.List(ctx, &scope1)
	if err != nil {
		t.Fatalf("List(scope-a): %v", err)
	}
	if len(runs) != 1 {
		t.Errorf("List(scope-a) count = %d, want 1", len(runs))
	}
}

func TestStore_AddEventAndEvents(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	id, _ := store.Create(ctx, &Run{ProjectDir: "/tmp", Goal: "test", Complexity: 3, AutoAdvance: true})

	gateResult := GatePass
	gateTier := TierNone
	err := store.AddEvent(ctx, &PhaseEvent{
		RunID:      id,
		FromPhase:  PhaseBrainstorm,
		ToPhase:    PhaseBrainstormReviewed,
		EventType:  EventAdvance,
		GateResult: &gateResult,
		GateTier:   &gateTier,
	})
	if err != nil {
		t.Fatalf("AddEvent: %v", err)
	}

	events, err := store.Events(ctx, id)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("Events count = %d, want 1", len(events))
	}

	e := events[0]
	if e.RunID != id {
		t.Errorf("RunID = %q, want %q", e.RunID, id)
	}
	if e.FromPhase != PhaseBrainstorm {
		t.Errorf("FromPhase = %q, want %q", e.FromPhase, PhaseBrainstorm)
	}
	if e.ToPhase != PhaseBrainstormReviewed {
		t.Errorf("ToPhase = %q, want %q", e.ToPhase, PhaseBrainstormReviewed)
	}
	if e.EventType != EventAdvance {
		t.Errorf("EventType = %q, want %q", e.EventType, EventAdvance)
	}
}

func TestStore_Events_Ordered(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	id, _ := store.Create(ctx, &Run{ProjectDir: "/tmp", Goal: "test", Complexity: 3, AutoAdvance: true})

	// Add events in order
	phases := []struct{ from, to string }{
		{PhaseBrainstorm, PhaseBrainstormReviewed},
		{PhaseBrainstormReviewed, PhaseStrategized},
		{PhaseStrategized, PhasePlanned},
	}
	for _, p := range phases {
		store.AddEvent(ctx, &PhaseEvent{
			RunID: id, FromPhase: p.from, ToPhase: p.to, EventType: EventAdvance,
		})
	}

	events, _ := store.Events(ctx, id)
	if len(events) != 3 {
		t.Fatalf("Events count = %d, want 3", len(events))
	}

	// Verify ordering: IDs should be strictly increasing
	for i := 1; i < len(events); i++ {
		if events[i].ID <= events[i-1].ID {
			t.Errorf("Events not ordered: [%d].ID=%d <= [%d].ID=%d", i, events[i].ID, i-1, events[i-1].ID)
		}
	}
}

func TestStore_Events_Empty(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	id, _ := store.Create(ctx, &Run{ProjectDir: "/tmp", Goal: "test", Complexity: 3, AutoAdvance: true})

	events, err := store.Events(ctx, id)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	// nil slice is fine — it just means no events yet
	if events != nil && len(events) != 0 {
		t.Errorf("Events for new run should be empty, got %d", len(events))
	}
}

// Ensure the internal helpers work correctly with the underlying SQL types.
func TestNullHelpers(t *testing.T) {
	if got := nullStr(sql.NullString{Valid: false}); got != nil {
		t.Errorf("nullStr(invalid) = %v, want nil", got)
	}
	if got := nullStr(sql.NullString{String: "hello", Valid: true}); got == nil || *got != "hello" {
		t.Errorf("nullStr(hello) = %v, want 'hello'", got)
	}
	if got := nullInt64(sql.NullInt64{Valid: false}); got != nil {
		t.Errorf("nullInt64(invalid) = %v, want nil", got)
	}
	if got := nullInt64(sql.NullInt64{Int64: 42, Valid: true}); got == nil || *got != 42 {
		t.Errorf("nullInt64(42) = %v, want 42", got)
	}
}
