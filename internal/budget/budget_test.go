package budget

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/mistakeknot/interverse/infra/intercore/internal/db"
	"github.com/mistakeknot/interverse/infra/intercore/internal/dispatch"
	"github.com/mistakeknot/interverse/infra/intercore/internal/phase"
	"github.com/mistakeknot/interverse/infra/intercore/internal/state"
)

func setupBudgetTest(t *testing.T) (*phase.Store, *dispatch.Store, *state.Store, context.Context) {
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

	return phase.New(d.SqlDB()), dispatch.New(d.SqlDB(), nil), state.New(d.SqlDB()), ctx
}

func int64Ptr(v int64) *int64 { return &v }

func TestBudgetWarning(t *testing.T) {
	ps, ds, ss, ctx := setupBudgetTest(t)

	// Create run with 10000 token budget, 80% warning
	runID, err := ps.Create(ctx, &phase.Run{
		ProjectDir:    "/tmp/test",
		Goal:          "test budget",
		TokenBudget:   int64Ptr(10000),
		BudgetWarnPct: 80,
		AutoAdvance:   true,
	})
	if err != nil {
		t.Fatal(err)
	}

	var emitted []string
	recorder := func(ctx context.Context, runID, eventType, reason string) error {
		emitted = append(emitted, eventType)
		return nil
	}

	checker := New(ps, ds, ss, recorder)

	// Add dispatch with 7000 tokens (below 80%)
	d := &dispatch.Dispatch{ProjectDir: "/tmp", AgentType: "codex", ScopeID: &runID}
	id1, _ := ds.Create(ctx, d)
	ds.UpdateStatus(ctx, id1, dispatch.StatusCompleted, dispatch.UpdateFields{
		"input_tokens": 7000, "output_tokens": 0,
	})

	result, err := checker.Check(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Warning {
		t.Error("Warning should be false (7000 < 8000)")
	}

	// Add dispatch pushing total to 9000 (above 80%)
	id2, _ := ds.Create(ctx, &dispatch.Dispatch{ProjectDir: "/tmp", AgentType: "codex", ScopeID: &runID})
	ds.UpdateStatus(ctx, id2, dispatch.StatusCompleted, dispatch.UpdateFields{
		"input_tokens": 2000, "output_tokens": 0,
	})

	result, err = checker.Check(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Warning {
		t.Error("Warning should be true (9000 >= 8000)")
	}
	if result.Exceeded {
		t.Error("Exceeded should be false (9000 < 10000)")
	}
	if len(emitted) != 1 || emitted[0] != EventBudgetWarning {
		t.Errorf("emitted = %v, want [budget.warning]", emitted)
	}

	// Second check should NOT re-emit (dedup via state)
	emitted = nil
	result, err = checker.Check(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Warning {
		t.Error("Warning should be false on second check (dedup)")
	}
	if len(emitted) != 0 {
		t.Errorf("should not re-emit, got %v", emitted)
	}
}

func TestBudgetExceeded(t *testing.T) {
	ps, ds, ss, ctx := setupBudgetTest(t)

	runID, err := ps.Create(ctx, &phase.Run{
		ProjectDir:    "/tmp/test",
		Goal:          "test exceeded",
		TokenBudget:   int64Ptr(5000),
		BudgetWarnPct: 80,
		AutoAdvance:   true,
	})
	if err != nil {
		t.Fatal(err)
	}

	var emitted []string
	recorder := func(ctx context.Context, runID, eventType, reason string) error {
		emitted = append(emitted, eventType)
		return nil
	}

	checker := New(ps, ds, ss, recorder)

	// Add dispatch with 6000 tokens (exceeds budget AND warning)
	id, _ := ds.Create(ctx, &dispatch.Dispatch{ProjectDir: "/tmp", AgentType: "codex", ScopeID: &runID})
	ds.UpdateStatus(ctx, id, dispatch.StatusCompleted, dispatch.UpdateFields{
		"input_tokens": 6000, "output_tokens": 0,
	})

	result, err := checker.Check(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Warning {
		t.Error("Warning should be true")
	}
	if !result.Exceeded {
		t.Error("Exceeded should be true")
	}
	if len(emitted) != 2 {
		t.Fatalf("emitted = %v, want 2 events", emitted)
	}
	if emitted[0] != EventBudgetWarning {
		t.Errorf("emitted[0] = %q, want %q", emitted[0], EventBudgetWarning)
	}
	if emitted[1] != EventBudgetExceeded {
		t.Errorf("emitted[1] = %q, want %q", emitted[1], EventBudgetExceeded)
	}
}

func TestNoBudget(t *testing.T) {
	ps, ds, ss, ctx := setupBudgetTest(t)

	// Run without budget
	runID, err := ps.Create(ctx, &phase.Run{
		ProjectDir:  "/tmp/test",
		Goal:        "no budget",
		AutoAdvance: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	checker := New(ps, ds, ss, nil)
	result, err := checker.Check(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if result != nil {
		t.Error("Result should be nil when no budget is set")
	}
}
