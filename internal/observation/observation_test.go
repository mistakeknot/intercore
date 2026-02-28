package observation

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/mistakeknot/intercore/internal/db"
	"github.com/mistakeknot/intercore/internal/dispatch"
	"github.com/mistakeknot/intercore/internal/event"
	"github.com/mistakeknot/intercore/internal/phase"
	"github.com/mistakeknot/intercore/internal/scheduler"
)

// testCollector creates a real SQLite test DB, migrates it, creates real
// phase/dispatch/event/scheduler stores, and returns a Collector wired to all of them.
func testCollector(t *testing.T) (*Collector, *db.DB) {
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

	pStore := phase.New(d.SqlDB())
	dStore := dispatch.New(d.SqlDB(), nil)
	eStore := event.NewStore(d.SqlDB())
	sStore := scheduler.NewStore(d.SqlDB())

	return NewCollector(pStore, dStore, eStore, sStore), d
}

func TestCollectReturnsSnapshot(t *testing.T) {
	c := NewCollector(nil, nil, nil, nil)
	snap, err := c.Collect(context.Background(), CollectOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if snap == nil {
		t.Fatal("expected non-nil snapshot")
	}
	if snap.Timestamp.IsZero() {
		t.Error("expected non-zero timestamp")
	}
	if snap.Runs == nil {
		t.Error("expected non-nil Runs slice")
	}
	if len(snap.Runs) != 0 {
		t.Errorf("expected empty Runs, got %d", len(snap.Runs))
	}
	if snap.Events == nil {
		t.Error("expected non-nil Events slice")
	}
	if len(snap.Events) != 0 {
		t.Errorf("expected empty Events, got %d", len(snap.Events))
	}
	if snap.Dispatches.Agents == nil {
		t.Error("expected non-nil Dispatches.Agents slice")
	}
	if len(snap.Dispatches.Agents) != 0 {
		t.Errorf("expected empty Dispatches.Agents, got %d", len(snap.Dispatches.Agents))
	}
	if snap.Budget != nil {
		t.Error("expected nil Budget")
	}
}

func TestCollectIntegration(t *testing.T) {
	c, _ := testCollector(t)
	ctx := context.Background()

	// Seed a run via the real phase store.
	// phase.Store.Create takes (*Run) and returns (id string, err error).
	pStore := c.phases.(*phase.Store)
	runID, err := pStore.Create(ctx, &phase.Run{
		ProjectDir: "/tmp/test-project",
		Goal:       "Implement feature X",
		Complexity: 3,
	})
	if err != nil {
		t.Fatalf("phase.Create: %v", err)
	}

	// Collect without scoping — should see the active run.
	snap, err := c.Collect(ctx, CollectOptions{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if len(snap.Runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(snap.Runs))
	}
	run := snap.Runs[0]
	if run.ID != runID {
		t.Errorf("run ID = %q, want %q", run.ID, runID)
	}
	if run.Phase != phase.PhaseBrainstorm {
		t.Errorf("run Phase = %q, want %q", run.Phase, phase.PhaseBrainstorm)
	}
	if run.Goal != "Implement feature X" {
		t.Errorf("run Goal = %q, want %q", run.Goal, "Implement feature X")
	}
	if run.Status != phase.StatusActive {
		t.Errorf("run Status = %q, want %q", run.Status, phase.StatusActive)
	}
	if run.ProjectDir != "/tmp/test-project" {
		t.Errorf("run ProjectDir = %q, want %q", run.ProjectDir, "/tmp/test-project")
	}
	if run.CreatedAt == 0 {
		t.Error("run CreatedAt should be non-zero")
	}

	// Snapshot metadata checks.
	if snap.Timestamp.IsZero() {
		t.Error("expected non-zero timestamp")
	}
	if snap.Budget != nil {
		t.Error("expected nil Budget when no RunID scope and no token budget")
	}
}

func TestCollectWithRunScope(t *testing.T) {
	c, _ := testCollector(t)
	ctx := context.Background()

	pStore := c.phases.(*phase.Store)

	// Create two runs.
	runID1, err := pStore.Create(ctx, &phase.Run{
		ProjectDir: "/tmp/project-a",
		Goal:       "Goal A",
	})
	if err != nil {
		t.Fatalf("Create run 1: %v", err)
	}
	runID2, err := pStore.Create(ctx, &phase.Run{
		ProjectDir: "/tmp/project-b",
		Goal:       "Goal B",
	})
	if err != nil {
		t.Fatalf("Create run 2: %v", err)
	}

	// Collect scoped to run 1.
	snap, err := c.Collect(ctx, CollectOptions{RunID: runID1})
	if err != nil {
		t.Fatalf("Collect with RunID: %v", err)
	}

	if len(snap.Runs) != 1 {
		t.Fatalf("expected 1 scoped run, got %d", len(snap.Runs))
	}
	if snap.Runs[0].ID != runID1 {
		t.Errorf("scoped run ID = %q, want %q", snap.Runs[0].ID, runID1)
	}
	if snap.Runs[0].Goal != "Goal A" {
		t.Errorf("scoped run Goal = %q, want %q", snap.Runs[0].Goal, "Goal A")
	}

	// Verify unscoped Collect returns both runs.
	snapAll, err := c.Collect(ctx, CollectOptions{})
	if err != nil {
		t.Fatalf("Collect all: %v", err)
	}
	if len(snapAll.Runs) != 2 {
		t.Errorf("expected 2 runs in unscoped Collect, got %d", len(snapAll.Runs))
	}

	_ = runID2 // used in Create above
}

func TestCollectIntegrationWithBudget(t *testing.T) {
	c, _ := testCollector(t)
	ctx := context.Background()

	pStore := c.phases.(*phase.Store)
	dStore := c.dispatches.(*dispatch.Store)

	// Create a run with a token budget.
	budget := int64(100000)
	runID, err := pStore.Create(ctx, &phase.Run{
		ProjectDir:  "/tmp/budget-project",
		Goal:        "Budget test",
		TokenBudget: &budget,
	})
	if err != nil {
		t.Fatalf("Create run: %v", err)
	}

	// Create a dispatch scoped to this run with some token usage.
	dispID, err := dStore.Create(ctx, &dispatch.Dispatch{
		AgentType:  "codex",
		ProjectDir: "/tmp/budget-project",
		ScopeID:    &runID,
	})
	if err != nil {
		t.Fatalf("Create dispatch: %v", err)
	}
	err = dStore.UpdateStatus(ctx, dispID, dispatch.StatusCompleted, dispatch.UpdateFields{
		"input_tokens":  5000,
		"output_tokens": 3000,
		"exit_code":     0,
	})
	if err != nil {
		t.Fatalf("UpdateStatus dispatch: %v", err)
	}

	// Collect scoped to the run — should include budget.
	snap, err := c.Collect(ctx, CollectOptions{RunID: runID})
	if err != nil {
		t.Fatalf("Collect with budget: %v", err)
	}

	if snap.Budget == nil {
		t.Fatal("expected non-nil Budget")
	}
	if snap.Budget.RunID != runID {
		t.Errorf("Budget.RunID = %q, want %q", snap.Budget.RunID, runID)
	}
	if snap.Budget.Budget != 100000 {
		t.Errorf("Budget.Budget = %d, want 100000", snap.Budget.Budget)
	}
	// Used = input_tokens + output_tokens = 5000 + 3000 = 8000
	if snap.Budget.Used != 8000 {
		t.Errorf("Budget.Used = %d, want 8000", snap.Budget.Used)
	}
	if snap.Budget.Remaining != 92000 {
		t.Errorf("Budget.Remaining = %d, want 92000", snap.Budget.Remaining)
	}
}

func TestCollectEmptyStores(t *testing.T) {
	c, _ := testCollector(t)
	ctx := context.Background()

	// Collect against real but empty stores — should return empty snapshot.
	snap, err := c.Collect(ctx, CollectOptions{})
	if err != nil {
		t.Fatalf("Collect empty: %v", err)
	}
	if len(snap.Runs) != 0 {
		t.Errorf("expected 0 runs, got %d", len(snap.Runs))
	}
	if len(snap.Events) != 0 {
		t.Errorf("expected 0 events, got %d", len(snap.Events))
	}
	if len(snap.Dispatches.Agents) != 0 {
		t.Errorf("expected 0 agents, got %d", len(snap.Dispatches.Agents))
	}
	if snap.Queue.Pending != 0 || snap.Queue.Running != 0 || snap.Queue.Retrying != 0 {
		t.Errorf("expected zero queue counts, got %+v", snap.Queue)
	}
}
