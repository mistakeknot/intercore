package phase

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mistakeknot/intercore/internal/db"
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

func TestStore_Current(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	id, _ := store.Create(ctx, &Run{
		ProjectDir: "/tmp/myproject", Goal: "test goal", Complexity: 3, AutoAdvance: true,
	})

	got, err := store.Current(ctx, "/tmp/myproject")
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if got.ID != id {
		t.Errorf("Current ID = %q, want %q", got.ID, id)
	}
	if got.Goal != "test goal" {
		t.Errorf("Goal = %q, want %q", got.Goal, "test goal")
	}
}

func TestStore_Current_NoActiveRun(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	_, err := store.Current(ctx, "/tmp/noproject")
	if err != ErrNotFound {
		t.Errorf("Current(noproject) error = %v, want ErrNotFound", err)
	}
}

func TestStore_Current_MostRecent(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	// Create two active runs for the same project.
	// Insert directly with explicit timestamps to guarantee ordering,
	// since Create() uses time.Now().Unix() which has second resolution.
	db := store.db
	_, err := db.ExecContext(ctx, `
		INSERT INTO runs (id, project_dir, goal, status, phase, complexity,
			force_full, auto_advance, created_at, updated_at)
		VALUES ('run_old', '/tmp/multi', 'first', 'active', 'brainstorm', 3, 0, 1, 1000, 1000)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO runs (id, project_dir, goal, status, phase, complexity,
			force_full, auto_advance, created_at, updated_at)
		VALUES ('run_new', '/tmp/multi', 'second', 'active', 'brainstorm', 3, 0, 1, 2000, 2000)`)
	if err != nil {
		t.Fatal(err)
	}

	got, err := store.Current(ctx, "/tmp/multi")
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	// Should return the most recent (run_new has created_at=2000)
	if got.ID != "run_new" {
		t.Errorf("Current ID = %q, want %q (most recent)", got.ID, "run_new")
	}
	if got.Goal != "second" {
		t.Errorf("Goal = %q, want %q", got.Goal, "second")
	}
}

func TestStore_Current_IgnoresCancelled(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	id1, _ := store.Create(ctx, &Run{
		ProjectDir: "/tmp/cancelled", Goal: "first", Complexity: 3, AutoAdvance: true,
	})
	id2, _ := store.Create(ctx, &Run{
		ProjectDir: "/tmp/cancelled", Goal: "second", Complexity: 3, AutoAdvance: true,
	})
	// Cancel the newest one
	store.UpdateStatus(ctx, id2, StatusCancelled)

	got, err := store.Current(ctx, "/tmp/cancelled")
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	// Should return the first (only active one)
	if got.ID != id1 {
		t.Errorf("Current ID = %q, want %q (only active)", got.ID, id1)
	}
}

func TestStore_SkipPhase(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	id, _ := store.Create(ctx, &Run{
		ProjectDir:  "/tmp",
		Goal:        "test skip",
		Phases:      []string{"a", "b", "c", "d"},
		AutoAdvance: true,
	})

	// Skip phase "b" while at phase "a"
	err := store.SkipPhase(ctx, id, "b", "complexity 1", "clavain")
	if err != nil {
		t.Fatalf("SkipPhase: %v", err)
	}

	// Verify event was recorded
	events, err := store.Events(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events {
		if e.EventType == EventSkip && e.ToPhase == "b" {
			found = true
			if e.Reason == nil || !strings.Contains(*e.Reason, "clavain") {
				t.Errorf("skip event reason = %v, want to contain 'clavain'", e.Reason)
			}
		}
	}
	if !found {
		t.Error("expected skip event for phase 'b'")
	}

	// Verify SkippedPhases returns the right set
	skipped, err := store.SkippedPhases(ctx, id)
	if err != nil {
		t.Fatalf("SkippedPhases: %v", err)
	}
	if !skipped["b"] {
		t.Error("expected 'b' in skipped set")
	}
	if skipped["c"] {
		t.Error("expected 'c' NOT in skipped set")
	}
}

func TestStore_SkipPhaseErrors(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	id, _ := store.Create(ctx, &Run{
		ProjectDir:  "/tmp",
		Goal:        "test skip errors",
		Phases:      []string{"a", "b", "c"},
		AutoAdvance: true,
	})

	// Skip nonexistent phase
	err := store.SkipPhase(ctx, id, "nonexistent", "test", "test")
	if err == nil {
		t.Error("expected error for nonexistent phase")
	}

	// Skip current phase (current is "a", can't skip "a" itself)
	err = store.SkipPhase(ctx, id, "a", "test", "test")
	if err == nil {
		t.Error("expected error for skipping current phase")
	}

	// Skip terminal phase
	err = store.SkipPhase(ctx, id, "c", "test", "test")
	if err == nil {
		t.Error("expected error for skipping terminal phase")
	}

	// Skip on nonexistent run
	err = store.SkipPhase(ctx, "nonexist", "b", "test", "test")
	if err != ErrNotFound {
		t.Errorf("SkipPhase(nonexist) error = %v, want ErrNotFound", err)
	}
}

func TestRollbackPhase(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	// Create a run and advance it twice: brainstorm → brainstorm-reviewed → strategized
	id, err := store.Create(ctx, &Run{
		ProjectDir: "/tmp/test", Goal: "test rollback", Complexity: 3, AutoAdvance: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdatePhase(ctx, id, PhaseBrainstorm, PhaseBrainstormReviewed); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdatePhase(ctx, id, PhaseBrainstormReviewed, PhaseStrategized); err != nil {
		t.Fatal(err)
	}

	// Rollback to brainstorm
	err = store.RollbackPhase(ctx, id, PhaseStrategized, PhaseBrainstorm)
	if err != nil {
		t.Fatalf("RollbackPhase failed: %v", err)
	}

	// Verify phase is now brainstorm
	run, err := store.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if run.Phase != PhaseBrainstorm {
		t.Fatalf("expected phase brainstorm, got %s", run.Phase)
	}
}

func TestRollbackPhase_StalePhase(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	id, err := store.Create(ctx, &Run{
		ProjectDir: "/tmp/test", Goal: "test", Complexity: 3, AutoAdvance: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Provide wrong currentPhase — should get ErrStalePhase
	err = store.RollbackPhase(ctx, id, "nonexistent-phase", PhaseBrainstorm)
	if err != ErrStalePhase {
		t.Fatalf("expected ErrStalePhase, got %v", err)
	}
}

func TestRollbackPhase_TerminalRun(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	id, err := store.Create(ctx, &Run{
		ProjectDir: "/tmp/test", Goal: "test", Complexity: 3, AutoAdvance: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateStatus(ctx, id, StatusCancelled); err != nil {
		t.Fatal(err)
	}

	// Cancelled run should return ErrTerminalRun
	err = store.RollbackPhase(ctx, id, PhaseBrainstorm, PhaseBrainstorm)
	if err != ErrTerminalRun {
		t.Fatalf("expected ErrTerminalRun, got %v", err)
	}
}

func TestRollbackPhase_CompletedRun(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	id, err := store.Create(ctx, &Run{
		ProjectDir: "/tmp/test", Goal: "test", Complexity: 3, AutoAdvance: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Advance to done and mark completed
	if err := store.UpdatePhase(ctx, id, PhaseBrainstorm, PhaseDone); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateStatus(ctx, id, StatusCompleted); err != nil {
		t.Fatal(err)
	}

	// Rollback should revert status to active
	err = store.RollbackPhase(ctx, id, PhaseDone, PhaseBrainstorm)
	if err != nil {
		t.Fatalf("RollbackPhase on completed run failed: %v", err)
	}

	run, err := store.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != StatusActive {
		t.Fatalf("expected status active, got %s", run.Status)
	}
	if run.CompletedAt != nil {
		t.Fatal("expected completed_at to be cleared after rollback")
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

func TestParseGateRules(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{
			name:  "valid single rule",
			input: `{"brainstorm→brainstorm-reviewed":[{"Check":"artifact_exists","Phase":"brainstorm","Tier":"hard"}]}`,
		},
		{
			name:  "valid multiple transitions",
			input: `{"brainstorm→brainstorm-reviewed":[{"Check":"artifact_exists","Phase":"brainstorm","Tier":"hard"}],"planned→executing":[{"Check":"artifact_exists","Phase":"planned","Tier":"soft"}]}`,
		},
		{
			name:  "valid empty tier",
			input: `{"brainstorm→brainstorm-reviewed":[{"Check":"artifact_exists","Phase":"brainstorm","Tier":""}]}`,
		},
		{
			name:  "empty map is valid",
			input: `{}`,
		},
		{
			name:    "invalid JSON",
			input:   `{bad`,
			wantErr: true,
		},
		{
			name:    "unknown check type",
			input:   `{"a→b":[{"Check":"does_not_exist","Phase":"","Tier":"hard"}]}`,
			wantErr: true,
		},
		{
			name:    "invalid tier",
			input:   `{"a→b":[{"Check":"artifact_exists","Phase":"","Tier":"medium"}]}`,
			wantErr: true,
		},
		{
			name:    "empty transition key",
			input:   `{"":[{"Check":"artifact_exists","Phase":"","Tier":"hard"}]}`,
			wantErr: true,
		},
		{
			name:    "invalid transition key format",
			input:   `{"brainstorm->brainstorm-reviewed":[{"Check":"artifact_exists","Phase":"brainstorm","Tier":"hard"}]}`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseGateRules(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("ParseGateRules(%s) = %v, want error", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Errorf("ParseGateRules(%s) error: %v", tt.input, err)
			}
		})
	}
}

func TestValidateGateRulesForChain(t *testing.T) {
	chain := []string{"brainstorm", "plan-reviewed", "shipping"}

	t.Run("full coverage with explicit ungated transition", func(t *testing.T) {
		rules := map[string][]SpecGateRule{
			"brainstorm→plan-reviewed": {
				{Check: CheckArtifactExists, Phase: "brainstorm", Tier: "hard"},
			},
			"plan-reviewed→shipping": {},
		}
		if err := ValidateGateRulesForChain(chain, rules); err != nil {
			t.Fatalf("ValidateGateRulesForChain: %v", err)
		}
	})

	t.Run("missing transition fails", func(t *testing.T) {
		rules := map[string][]SpecGateRule{
			"brainstorm→plan-reviewed": {
				{Check: CheckArtifactExists, Phase: "brainstorm", Tier: "hard"},
			},
		}
		err := ValidateGateRulesForChain(chain, rules)
		if err == nil {
			t.Fatal("expected error for missing transition coverage")
		}
		if !strings.Contains(err.Error(), "missing transitions") {
			t.Fatalf("error = %v, want missing transitions message", err)
		}
	})

	t.Run("non-adjacent transition fails", func(t *testing.T) {
		rules := map[string][]SpecGateRule{
			"brainstorm→plan-reviewed": {
				{Check: CheckArtifactExists, Phase: "brainstorm", Tier: "hard"},
			},
			"plan-reviewed→shipping": {},
			"brainstorm→shipping": {},
		}
		err := ValidateGateRulesForChain(chain, rules)
		if err == nil {
			t.Fatal("expected error for non-adjacent transition")
		}
		if !strings.Contains(err.Error(), "not adjacent") {
			t.Fatalf("error = %v, want not adjacent message", err)
		}
	})

	t.Run("transition not in chain fails", func(t *testing.T) {
		rules := map[string][]SpecGateRule{
			"brainstorm→plan-reviewed": {
				{Check: CheckArtifactExists, Phase: "brainstorm", Tier: "hard"},
			},
			"plan-reviewed→shipping": {},
			"review→done": {},
		}
		err := ValidateGateRulesForChain(chain, rules)
		if err == nil {
			t.Fatal("expected error for transition outside chain")
		}
		if !strings.Contains(err.Error(), "not present in active phase chain") {
			t.Fatalf("error = %v, want not present message", err)
		}
	})
}

func TestStore_CreateAndGetWithGateRules(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	rules := map[string][]SpecGateRule{
		"brainstorm→brainstorm-reviewed": {
			{Check: CheckArtifactExists, Phase: "brainstorm", Tier: "hard"},
		},
		"planned→executing": {
			{Check: CheckArtifactExists, Phase: "planned", Tier: "soft"},
			{Check: CheckAgentsComplete, Tier: "hard"},
		},
	}

	id, err := store.Create(ctx, &Run{
		ProjectDir: "/test",
		Goal:       "test gate rules",
		GateRules:  rules,
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := store.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}

	if got.GateRules == nil {
		t.Fatal("GateRules is nil after round-trip")
	}
	if len(got.GateRules) != 2 {
		t.Errorf("GateRules has %d transitions, want 2", len(got.GateRules))
	}
	br := got.GateRules["brainstorm→brainstorm-reviewed"]
	if len(br) != 1 || br[0].Check != CheckArtifactExists {
		t.Errorf("brainstorm rules = %v, want 1 artifact_exists", br)
	}
	pe := got.GateRules["planned→executing"]
	if len(pe) != 2 {
		t.Errorf("planned→executing rules = %v, want 2 rules", pe)
	}
}

func TestStore_CreateWithoutGateRules(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	id, err := store.Create(ctx, &Run{
		ProjectDir: "/test",
		Goal:       "no gate rules",
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := store.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}

	if got.GateRules != nil {
		t.Errorf("GateRules = %v, want nil for run without custom gates", got.GateRules)
	}
}
