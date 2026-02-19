package phase

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/mistakeknot/interverse/infra/intercore/internal/db"
	"github.com/mistakeknot/interverse/infra/intercore/internal/runtrack"
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
		if IsTerminalPhase(run.Phase) || IsTerminalStatus(run.Status) {
			t.Fatalf("advanceToPhase(%s): overshot (currently at %s, status %s)", target, run.Phase, run.Status)
		}
		result, err := Advance(context.Background(), store, runID, cfg, rt, nil, nil)
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

	result, err := Advance(ctx, store, id, GateConfig{Priority: 4}, nil, nil, nil)
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

func TestAdvance_WithComplexitySkip(t *testing.T) {
	store, _, _, ctx := setupMachineTest(t)

	id, _ := store.Create(ctx, &Run{
		ProjectDir: "/tmp", Goal: "test", Complexity: 1, AutoAdvance: true,
	})

	// At complexity 1: brainstorm → planned (skips brainstorm-reviewed + strategized)
	result, err := Advance(ctx, store, id, GateConfig{Priority: 4}, nil, nil, nil)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}

	if result.ToPhase != PhasePlanned {
		t.Errorf("ToPhase = %q, want %q (skip)", result.ToPhase, PhasePlanned)
	}
	if result.EventType != EventSkip {
		t.Errorf("EventType = %q, want %q", result.EventType, EventSkip)
	}
}

func TestAdvance_ForceFull_OverridesSkip(t *testing.T) {
	store, _, _, ctx := setupMachineTest(t)

	id, _ := store.Create(ctx, &Run{
		ProjectDir: "/tmp", Goal: "test", Complexity: 1,
		AutoAdvance: true, ForceFull: true,
	})

	// Even at complexity 1, force_full means brainstorm → brainstorm-reviewed
	result, err := Advance(ctx, store, id, GateConfig{Priority: 4}, nil, nil, nil)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}

	if result.ToPhase != PhaseBrainstormReviewed {
		t.Errorf("ToPhase = %q, want %q (force full)", result.ToPhase, PhaseBrainstormReviewed)
	}
	if result.EventType != EventAdvance {
		t.Errorf("EventType = %q, want %q", result.EventType, EventAdvance)
	}
}

func TestAdvance_AutoAdvanceFalse_Pauses(t *testing.T) {
	store, _, _, ctx := setupMachineTest(t)

	id, _ := store.Create(ctx, &Run{
		ProjectDir: "/tmp", Goal: "test", Complexity: 3, AutoAdvance: false,
	})

	result, err := Advance(ctx, store, id, GateConfig{Priority: 4}, nil, nil, nil)
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
	}, nil, nil, nil)
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
	r1, _ := Advance(ctx, store, id1, GateConfig{Priority: 4}, nil, nil, nil)
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
	r2, _ := Advance(ctx, store, id2, GateConfig{Priority: 2}, rtStore, nil, nil)
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
	r3, _ := Advance(ctx, store, id3, GateConfig{Priority: 0}, rtStore, nil, nil)
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
	r4, _ := Advance(ctx, store, id4, GateConfig{Priority: 0, DisableAll: true}, nil, nil, nil)
	if r4.GateTier != TierNone {
		t.Errorf("DisableAll tier = %q, want %q", r4.GateTier, TierNone)
	}
}

func TestAdvance_ToDone_CompletesRun(t *testing.T) {
	store, _, _, ctx := setupMachineTest(t)

	// Complexity 1: brainstorm → planned → executing → done
	id, _ := store.Create(ctx, &Run{
		ProjectDir: "/tmp", Goal: "test", Complexity: 1, AutoAdvance: true,
	})

	cfg := GateConfig{Priority: 4}

	// brainstorm → planned
	Advance(ctx, store, id, cfg, nil, nil, nil)
	// planned → executing
	Advance(ctx, store, id, cfg, nil, nil, nil)
	// executing → done
	result, err := Advance(ctx, store, id, cfg, nil, nil, nil)
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

	_, err := Advance(ctx, store, id, GateConfig{Priority: 4}, nil, nil, nil)
	if err != ErrTerminalRun {
		t.Errorf("Advance(cancelled run) error = %v, want ErrTerminalRun", err)
	}
}

func TestAdvance_TerminalPhase_ReturnsError(t *testing.T) {
	store, _, _, ctx := setupMachineTest(t)

	// Walk a complexity-1 run to done
	id, _ := store.Create(ctx, &Run{
		ProjectDir: "/tmp", Goal: "test", Complexity: 1, AutoAdvance: true,
	})
	cfg := GateConfig{Priority: 4}
	Advance(ctx, store, id, cfg, nil, nil, nil) // → planned
	Advance(ctx, store, id, cfg, nil, nil, nil) // → executing
	Advance(ctx, store, id, cfg, nil, nil, nil) // → done

	_, err := Advance(ctx, store, id, GateConfig{Priority: 4}, nil, nil, nil)
	if err != ErrTerminalRun {
		// Run is now completed, so ErrTerminalRun is expected
		t.Errorf("Advance(done run) error = %v, want ErrTerminalRun", err)
	}
}

func TestAdvance_NotFound(t *testing.T) {
	store, _, _, ctx := setupMachineTest(t)

	_, err := Advance(ctx, store, "nonexist", GateConfig{Priority: 4}, nil, nil, nil)
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
		PhaseDone,
	}

	cfg := GateConfig{Priority: 4}
	for i, expected := range expectedPhases {
		result, err := Advance(ctx, store, id, cfg, nil, nil, nil)
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
	if len(events) != 7 {
		t.Errorf("Events count = %d, want 7", len(events))
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
	}, nil, nil, nil)
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
