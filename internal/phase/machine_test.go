package phase

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/mistakeknot/intercore/internal/db"
	"github.com/mistakeknot/intercore/internal/runtrack"
)

func setupMachineTest(t *testing.T) (*Store, *runtrack.Store, *sql.DB, context.Context) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	d, err := db.Open(path, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	ctx := context.Background()
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	return New(d.SqlDB()), runtrack.New(d.SqlDB()), d.SqlDB(), ctx
}

func advanceToPhase(t *testing.T, store *Store, runID string, target string, rt RuntrackQuerier) {
	t.Helper()
	cfg := GateConfig{Priority: 4} // TierNone — bypass gates
	for {
		run, err := store.Get(context.Background(), runID)
		if err != nil {
			t.Fatalf("advanceToPhase(%s): get: %v", target, err)
		}
		if run.Phase == target {
			return
		}
		chain := ResolveChain(run)
		if ChainIsTerminal(chain, run.Phase) || IsTerminalStatus(run.Status) {
			t.Fatalf("advanceToPhase(%s): overshot (currently at %s, status %s)", target, run.Phase, run.Status)
		}
		result, err := Advance(context.Background(), store, runID, cfg, rt, nil, nil, nil, nil, nil)
		if err != nil {
			t.Fatalf("advanceToPhase(%s): %v", target, err)
		}
		if !result.Advanced {
			t.Fatalf("advanceToPhase(%s): advance returned Advanced=false at %s", target, run.Phase)
		}
	}
}

func TestAdvance_Basic(t *testing.T) {
	store, _, _, ctx := setupMachineTest(t)

	id, _ := store.Create(ctx, &Run{
		ProjectDir: "/tmp", Goal: "test", Complexity: 3, AutoAdvance: true,
	})

	result, err := Advance(ctx, store, id, GateConfig{Priority: 4}, nil, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}

	if !result.Advanced {
		t.Error("Advanced = false, want true")
	}
	if result.FromPhase != PhaseBrainstorm {
		t.Errorf("FromPhase = %q, want %q", result.FromPhase, PhaseBrainstorm)
	}
	if result.ToPhase != PhaseBrainstormReviewed {
		t.Errorf("ToPhase = %q, want %q", result.ToPhase, PhaseBrainstormReviewed)
	}
	if result.EventType != EventAdvance {
		t.Errorf("EventType = %q, want %q", result.EventType, EventAdvance)
	}

	// Verify run state was updated
	run, _ := store.Get(ctx, id)
	if run.Phase != PhaseBrainstormReviewed {
		t.Errorf("Run phase = %q, want %q", run.Phase, PhaseBrainstormReviewed)
	}

	// Verify event was recorded
	events, _ := store.Events(ctx, id)
	if len(events) != 1 {
		t.Errorf("Events count = %d, want 1", len(events))
	}
}

func TestAdvance_DefaultChain_StepsSequentially(t *testing.T) {
	store, _, _, ctx := setupMachineTest(t)

	// Even at complexity 1, Advance now always steps to next in chain.
	// Complexity-based skipping is handled by the explicit Skip command (Task 4).
	id, _ := store.Create(ctx, &Run{
		ProjectDir: "/tmp", Goal: "test", Complexity: 1, AutoAdvance: true,
	})

	result, err := Advance(ctx, store, id, GateConfig{Priority: 4}, nil, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}

	if result.ToPhase != PhaseBrainstormReviewed {
		t.Errorf("ToPhase = %q, want %q (sequential)", result.ToPhase, PhaseBrainstormReviewed)
	}
	if result.EventType != EventAdvance {
		t.Errorf("EventType = %q, want %q", result.EventType, EventAdvance)
	}
}

func TestAdvance_CustomChain(t *testing.T) {
	store, _, _, ctx := setupMachineTest(t)

	// Create a run with a custom 3-phase chain
	id, _ := store.Create(ctx, &Run{
		ProjectDir:  "/tmp",
		Goal:        "test custom chain",
		Phases:      []string{"draft", "review", "ship"},
		AutoAdvance: true,
	})

	// Initial phase should be first in chain
	run, _ := store.Get(ctx, id)
	if run.Phase != "draft" {
		t.Errorf("initial phase = %q, want %q", run.Phase, "draft")
	}

	cfg := GateConfig{Priority: 4}

	// draft → review
	r1, err := Advance(ctx, store, id, cfg, nil, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Advance 1: %v", err)
	}
	if r1.ToPhase != "review" {
		t.Errorf("advance 1 to = %q, want %q", r1.ToPhase, "review")
	}

	// review → ship (terminal — should complete the run)
	r2, err := Advance(ctx, store, id, cfg, nil, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Advance 2: %v", err)
	}
	if r2.ToPhase != "ship" {
		t.Errorf("advance 2 to = %q, want %q", r2.ToPhase, "ship")
	}

	// Run should be completed (ship is terminal)
	run, _ = store.Get(ctx, id)
	if run.Status != StatusCompleted {
		t.Errorf("status = %q, want %q", run.Status, StatusCompleted)
	}

	// Further advance should fail
	_, err = Advance(ctx, store, id, cfg, nil, nil, nil, nil, nil, nil)
	if err != ErrTerminalRun {
		t.Errorf("advance past terminal: err = %v, want ErrTerminalRun", err)
	}
}

func TestAdvance_AutoAdvanceFalse_Pauses(t *testing.T) {
	store, _, _, ctx := setupMachineTest(t)

	id, _ := store.Create(ctx, &Run{
		ProjectDir: "/tmp", Goal: "test", Complexity: 3, AutoAdvance: false,
	})

	result, err := Advance(ctx, store, id, GateConfig{Priority: 4}, nil, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}

	if result.Advanced {
		t.Error("Advanced = true, want false (auto_advance=false)")
	}
	if result.EventType != EventPause {
		t.Errorf("EventType = %q, want %q", result.EventType, EventPause)
	}

	// Phase should NOT have changed
	run, _ := store.Get(ctx, id)
	if run.Phase != PhaseBrainstorm {
		t.Errorf("Phase = %q, want %q (should not advance)", run.Phase, PhaseBrainstorm)
	}
}

func TestAdvance_AutoAdvanceFalse_WithSkipReason_Proceeds(t *testing.T) {
	store, _, _, ctx := setupMachineTest(t)

	id, _ := store.Create(ctx, &Run{
		ProjectDir: "/tmp", Goal: "test", Complexity: 3, AutoAdvance: false,
	})

	result, err := Advance(ctx, store, id, GateConfig{
		Priority:   4,
		SkipReason: "manual override",
	}, nil, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}

	if !result.Advanced {
		t.Error("Advanced = false, want true (skip reason overrides auto_advance)")
	}
}

func TestAdvance_GateTiers(t *testing.T) {
	store, rtStore, _, ctx := setupMachineTest(t)

	// Priority 4+ = no gate
	id1, _ := store.Create(ctx, &Run{
		ProjectDir: "/tmp", Goal: "t1", Complexity: 3, AutoAdvance: true,
	})
	r1, _ := Advance(ctx, store, id1, GateConfig{Priority: 4}, nil, nil, nil, nil, nil, nil)
	if r1.GateTier != TierNone {
		t.Errorf("Priority 4 tier = %q, want %q", r1.GateTier, TierNone)
	}

	// Priority 2-3 = soft gate — needs brainstorm artifact to pass
	id2, _ := store.Create(ctx, &Run{
		ProjectDir: "/tmp", Goal: "t2", Complexity: 3, AutoAdvance: true,
	})
	rtStore.AddArtifact(ctx, &runtrack.Artifact{
		RunID: id2, Phase: PhaseBrainstorm, Path: "test.md", Type: "file",
	})
	r2, _ := Advance(ctx, store, id2, GateConfig{Priority: 2}, rtStore, nil, nil, nil, nil, nil)
	if r2.GateTier != TierSoft {
		t.Errorf("Priority 2 tier = %q, want %q", r2.GateTier, TierSoft)
	}
	if !r2.Advanced {
		t.Error("Soft gate should allow advance when artifact exists")
	}

	// Priority 0-1 = hard gate — needs brainstorm artifact to pass
	id3, _ := store.Create(ctx, &Run{
		ProjectDir: "/tmp", Goal: "t3", Complexity: 3, AutoAdvance: true,
	})
	rtStore.AddArtifact(ctx, &runtrack.Artifact{
		RunID: id3, Phase: PhaseBrainstorm, Path: "test.md", Type: "file",
	})
	r3, _ := Advance(ctx, store, id3, GateConfig{Priority: 0}, rtStore, nil, nil, nil, nil, nil)
	if r3.GateTier != TierHard {
		t.Errorf("Priority 0 tier = %q, want %q", r3.GateTier, TierHard)
	}
	if !r3.Advanced {
		t.Error("Hard gate should pass when artifact exists")
	}

	// DisableAll = no gate
	id4, _ := store.Create(ctx, &Run{
		ProjectDir: "/tmp", Goal: "t4", Complexity: 3, AutoAdvance: true,
	})
	r4, _ := Advance(ctx, store, id4, GateConfig{Priority: 0, DisableAll: true}, nil, nil, nil, nil, nil, nil)
	if r4.GateTier != TierNone {
		t.Errorf("DisableAll tier = %q, want %q", r4.GateTier, TierNone)
	}
}

func TestAdvance_ToDone_CompletesRun(t *testing.T) {
	store, _, _, ctx := setupMachineTest(t)

	id, _ := store.Create(ctx, &Run{
		ProjectDir: "/tmp", Goal: "test", Complexity: 3, AutoAdvance: true,
	})

	cfg := GateConfig{Priority: 4}

	// Walk through all 9 transitions: brainstorm → ... → done
	for i := 0; i < 7; i++ {
		_, err := Advance(ctx, store, id, cfg, nil, nil, nil, nil, nil, nil)
		if err != nil {
			t.Fatalf("Advance step %d: %v", i, err)
		}
	}

	// Final advance → done
	result, err := Advance(ctx, store, id, cfg, nil, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Advance to done: %v", err)
	}

	if result.ToPhase != PhaseDone {
		t.Errorf("ToPhase = %q, want %q", result.ToPhase, PhaseDone)
	}

	// Run should be completed
	run, _ := store.Get(ctx, id)
	if run.Status != StatusCompleted {
		t.Errorf("Status = %q, want %q", run.Status, StatusCompleted)
	}
	if run.CompletedAt == nil {
		t.Error("CompletedAt should be set")
	}
}

func TestAdvance_TerminalRun_ReturnsError(t *testing.T) {
	store, _, _, ctx := setupMachineTest(t)

	id, _ := store.Create(ctx, &Run{
		ProjectDir: "/tmp", Goal: "test", Complexity: 3, AutoAdvance: true,
	})
	store.UpdateStatus(ctx, id, StatusCancelled)

	_, err := Advance(ctx, store, id, GateConfig{Priority: 4}, nil, nil, nil, nil, nil, nil)
	if err != ErrTerminalRun {
		t.Errorf("Advance(cancelled run) error = %v, want ErrTerminalRun", err)
	}
}

func TestAdvance_TerminalPhase_ReturnsError(t *testing.T) {
	store, _, _, ctx := setupMachineTest(t)

	// Use a short custom chain to reach terminal quickly
	id, _ := store.Create(ctx, &Run{
		ProjectDir: "/tmp", Goal: "test", AutoAdvance: true,
		Phases: []string{"start", "done"},
	})
	cfg := GateConfig{Priority: 4}
	Advance(ctx, store, id, cfg, nil, nil, nil, nil, nil, nil) // → done (completed)

	_, err := Advance(ctx, store, id, GateConfig{Priority: 4}, nil, nil, nil, nil, nil, nil)
	if err != ErrTerminalRun {
		// Run is now completed, so ErrTerminalRun is expected
		t.Errorf("Advance(done run) error = %v, want ErrTerminalRun", err)
	}
}

func TestAdvance_NotFound(t *testing.T) {
	store, _, _, ctx := setupMachineTest(t)

	_, err := Advance(ctx, store, "nonexist", GateConfig{Priority: 4}, nil, nil, nil, nil, nil, nil)
	if err != ErrNotFound {
		t.Errorf("Advance(nonexist) error = %v, want ErrNotFound", err)
	}
}

func TestAdvance_FullLifecycle_Complexity3(t *testing.T) {
	store, _, _, ctx := setupMachineTest(t)

	id, _ := store.Create(ctx, &Run{
		ProjectDir: "/tmp", Goal: "full lifecycle", Complexity: 3, AutoAdvance: true,
	})

	expectedPhases := []string{
		PhaseBrainstormReviewed,
		PhaseStrategized,
		PhasePlanned,
		PhaseExecuting,
		PhaseReview,
		PhasePolish,
		PhaseReflect,
		PhaseDone,
	}

	cfg := GateConfig{Priority: 4}
	for i, expected := range expectedPhases {
		result, err := Advance(ctx, store, id, cfg, nil, nil, nil, nil, nil, nil)
		if err != nil {
			t.Fatalf("Advance step %d: %v", i, err)
		}
		if result.ToPhase != expected {
			t.Errorf("Step %d: ToPhase = %q, want %q", i, result.ToPhase, expected)
		}
		if !result.Advanced {
			t.Errorf("Step %d: Advanced = false", i)
		}
	}

	// Verify run is completed
	run, _ := store.Get(ctx, id)
	if run.Status != StatusCompleted {
		t.Errorf("Final status = %q, want %q", run.Status, StatusCompleted)
	}

	// Verify event trail
	events, _ := store.Events(ctx, id)
	if len(events) != 8 {
		t.Errorf("Events count = %d, want 8", len(events))
	}
}

func TestAdvance_SkipReason_RecordedInEvent(t *testing.T) {
	store, _, _, ctx := setupMachineTest(t)

	id, _ := store.Create(ctx, &Run{
		ProjectDir: "/tmp", Goal: "test", Complexity: 3, AutoAdvance: true,
	})

	result, err := Advance(ctx, store, id, GateConfig{
		Priority:   4,
		SkipReason: "testing reason",
	}, nil, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if result.Reason != "testing reason" {
		t.Errorf("Reason = %q, want %q", result.Reason, "testing reason")
	}

	events, _ := store.Events(ctx, id)
	if len(events) != 1 {
		t.Fatalf("Events count = %d, want 1", len(events))
	}
	if events[0].Reason == nil || *events[0].Reason != "testing reason" {
		t.Errorf("Event reason = %v, want 'testing reason'", events[0].Reason)
	}
}

func TestRollback_Basic(t *testing.T) {
	store, _, _, ctx := setupMachineTest(t)

	id, _ := store.Create(ctx, &Run{
		ProjectDir: "/tmp/test", Goal: "test rollback", Complexity: 3, AutoAdvance: true,
	})
	// Advance to strategized
	advanceToPhase(t, store, id, PhaseStrategized, nil)

	var callbackCalled bool
	callback := func(runID, eventType, fromPhase, toPhase, reason string) {
		callbackCalled = true
		if eventType != EventRollback {
			t.Errorf("callback event type = %q, want %q", eventType, EventRollback)
		}
	}

	result, err := Rollback(ctx, store, id, PhaseBrainstorm, "test reason", callback)
	if err != nil {
		t.Fatalf("Rollback failed: %v", err)
	}

	if result.FromPhase != PhaseStrategized {
		t.Errorf("FromPhase = %q, want %q", result.FromPhase, PhaseStrategized)
	}
	if result.ToPhase != PhaseBrainstorm {
		t.Errorf("ToPhase = %q, want %q", result.ToPhase, PhaseBrainstorm)
	}
	if !callbackCalled {
		t.Error("callback was not called")
	}

	// Verify the run's phase was updated
	run, err := store.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if run.Phase != PhaseBrainstorm {
		t.Errorf("run.Phase = %q, want %q", run.Phase, PhaseBrainstorm)
	}

	// Verify rollback event was recorded
	events, err := store.Events(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range events {
		if e.EventType == EventRollback {
			found = true
			if e.FromPhase != PhaseStrategized || e.ToPhase != PhaseBrainstorm {
				t.Errorf("rollback event: from=%q to=%q, want from=strategized to=brainstorm", e.FromPhase, e.ToPhase)
			}
		}
	}
	if !found {
		t.Error("no rollback event found in audit trail")
	}
}

func TestRollback_TerminalRun(t *testing.T) {
	store, _, _, ctx := setupMachineTest(t)

	id, _ := store.Create(ctx, &Run{
		ProjectDir: "/tmp/test", Goal: "test", Complexity: 3, AutoAdvance: true,
	})
	store.UpdateStatus(ctx, id, StatusCancelled)

	_, err := Rollback(ctx, store, id, PhaseBrainstorm, "test", nil)
	if err == nil {
		t.Fatal("expected error for cancelled run rollback")
	}
}

func TestRollback_RolledBackPhases(t *testing.T) {
	store, _, _, ctx := setupMachineTest(t)

	id, _ := store.Create(ctx, &Run{
		ProjectDir: "/tmp/test", Goal: "test", Complexity: 3, AutoAdvance: true,
	})
	advanceToPhase(t, store, id, PhasePlanned, nil)

	result, err := Rollback(ctx, store, id, PhaseBrainstorm, "test", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Should report 3 rolled-back phases: brainstorm-reviewed, strategized, planned
	if len(result.RolledBackPhases) != 3 {
		t.Fatalf("RolledBackPhases = %v, want 3 phases", result.RolledBackPhases)
	}
}
