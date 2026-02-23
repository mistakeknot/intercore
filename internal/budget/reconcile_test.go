package budget

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/mistakeknot/interverse/infra/intercore/internal/db"
	"github.com/mistakeknot/interverse/infra/intercore/internal/dispatch"
	"github.com/mistakeknot/interverse/infra/intercore/internal/phase"
)

func setupReconcileTest(t *testing.T) (*dispatch.Store, *ReconcileStore, *phase.Store, context.Context) {
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

	ds := dispatch.New(d.SqlDB(), nil)
	rs := NewReconcileStore(d.SqlDB(), ds)
	ps := phase.New(d.SqlDB())
	return ds, rs, ps, ctx
}

func TestReconcileNoDelta(t *testing.T) {
	ds, rs, ps, ctx := setupReconcileTest(t)

	runID, err := ps.Create(ctx, &phase.Run{
		ProjectDir:  "/tmp/test",
		Goal:        "reconcile test",
		AutoAdvance: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Create dispatch with known token counts
	id, _ := ds.Create(ctx, &dispatch.Dispatch{ProjectDir: "/tmp", AgentType: "codex", ScopeID: &runID})
	ds.UpdateStatus(ctx, id, dispatch.StatusCompleted, dispatch.UpdateFields{
		"input_tokens": 1000, "output_tokens": 500,
	})

	var emitted []string
	recorder := func(ctx context.Context, runID, eventType, reason string) error {
		emitted = append(emitted, eventType)
		return nil
	}

	// Reconcile with matching billed amounts
	rec, err := rs.Reconcile(ctx, runID, "", 1000, 500, "manual", recorder)
	if err != nil {
		t.Fatal(err)
	}

	if rec.DeltaIn != 0 {
		t.Errorf("DeltaIn = %d, want 0", rec.DeltaIn)
	}
	if rec.DeltaOut != 0 {
		t.Errorf("DeltaOut = %d, want 0", rec.DeltaOut)
	}
	if len(emitted) != 0 {
		t.Errorf("emitted = %v, want empty (no discrepancy)", emitted)
	}
}

func TestReconcileWithDiscrepancy(t *testing.T) {
	ds, rs, ps, ctx := setupReconcileTest(t)

	runID, err := ps.Create(ctx, &phase.Run{
		ProjectDir:  "/tmp/test",
		Goal:        "discrepancy test",
		AutoAdvance: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Create two dispatches
	id1, _ := ds.Create(ctx, &dispatch.Dispatch{ProjectDir: "/tmp", AgentType: "codex", ScopeID: &runID})
	ds.UpdateStatus(ctx, id1, dispatch.StatusCompleted, dispatch.UpdateFields{
		"input_tokens": 1000, "output_tokens": 200,
	})
	id2, _ := ds.Create(ctx, &dispatch.Dispatch{ProjectDir: "/tmp", AgentType: "codex", ScopeID: &runID})
	ds.UpdateStatus(ctx, id2, dispatch.StatusCompleted, dispatch.UpdateFields{
		"input_tokens": 2000, "output_tokens": 300,
	})

	var emitted []string
	recorder := func(ctx context.Context, runID, eventType, reason string) error {
		emitted = append(emitted, eventType)
		return nil
	}

	// Billed amounts differ from reported (3000/500 reported, 3500/600 billed)
	rec, err := rs.Reconcile(ctx, runID, "", 3500, 600, "anthropic", recorder)
	if err != nil {
		t.Fatal(err)
	}

	if rec.ReportedIn != 3000 {
		t.Errorf("ReportedIn = %d, want 3000", rec.ReportedIn)
	}
	if rec.ReportedOut != 500 {
		t.Errorf("ReportedOut = %d, want 500", rec.ReportedOut)
	}
	if rec.DeltaIn != 500 {
		t.Errorf("DeltaIn = %d, want 500", rec.DeltaIn)
	}
	if rec.DeltaOut != 100 {
		t.Errorf("DeltaOut = %d, want 100", rec.DeltaOut)
	}
	if rec.Source != "anthropic" {
		t.Errorf("Source = %q, want %q", rec.Source, "anthropic")
	}
	if len(emitted) != 1 || emitted[0] != EventCostDiscrepancy {
		t.Errorf("emitted = %v, want [%s]", emitted, EventCostDiscrepancy)
	}
}

func TestReconcileDispatchLevel(t *testing.T) {
	ds, rs, ps, ctx := setupReconcileTest(t)

	runID, err := ps.Create(ctx, &phase.Run{
		ProjectDir:  "/tmp/test",
		Goal:        "dispatch-level test",
		AutoAdvance: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Create dispatch with known tokens
	dID, _ := ds.Create(ctx, &dispatch.Dispatch{ProjectDir: "/tmp", AgentType: "codex", ScopeID: &runID})
	ds.UpdateStatus(ctx, dID, dispatch.StatusCompleted, dispatch.UpdateFields{
		"input_tokens": 5000, "output_tokens": 1000,
	})

	rec, err := rs.Reconcile(ctx, runID, dID, 5200, 1000, "manual", nil)
	if err != nil {
		t.Fatal(err)
	}

	if rec.DispatchID != dID {
		t.Errorf("DispatchID = %q, want %q", rec.DispatchID, dID)
	}
	if rec.ReportedIn != 5000 {
		t.Errorf("ReportedIn = %d, want 5000", rec.ReportedIn)
	}
	if rec.DeltaIn != 200 {
		t.Errorf("DeltaIn = %d, want 200", rec.DeltaIn)
	}
	if rec.DeltaOut != 0 {
		t.Errorf("DeltaOut = %d, want 0", rec.DeltaOut)
	}
}

func TestReconcileList(t *testing.T) {
	ds, rs, ps, ctx := setupReconcileTest(t)

	runID, err := ps.Create(ctx, &phase.Run{
		ProjectDir:  "/tmp/test",
		Goal:        "list test",
		AutoAdvance: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	dID, _ := ds.Create(ctx, &dispatch.Dispatch{ProjectDir: "/tmp", AgentType: "codex", ScopeID: &runID})
	ds.UpdateStatus(ctx, dID, dispatch.StatusCompleted, dispatch.UpdateFields{
		"input_tokens": 100, "output_tokens": 50,
	})

	// Insert two reconciliations
	_, err = rs.Reconcile(ctx, runID, "", 100, 50, "manual", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = rs.Reconcile(ctx, runID, "", 200, 50, "anthropic", nil)
	if err != nil {
		t.Fatal(err)
	}

	recs, err := rs.List(ctx, runID, 10)
	if err != nil {
		t.Fatal(err)
	}

	if len(recs) != 2 {
		t.Fatalf("List returned %d, want 2", len(recs))
	}

	// Most recent first (DESC order)
	if recs[0].Source != "anthropic" {
		t.Errorf("recs[0].Source = %q, want %q", recs[0].Source, "anthropic")
	}
	if recs[1].Source != "manual" {
		t.Errorf("recs[1].Source = %q, want %q", recs[1].Source, "manual")
	}

	// Most recent (id DESC) is the second reconciliation: billed 200 vs reported 100
	if recs[0].DeltaIn != 100 {
		t.Errorf("recs[0].DeltaIn = %d, want 100", recs[0].DeltaIn)
	}
	// First reconciliation: billed 100 = reported 100, no discrepancy
	if recs[1].DeltaIn != 0 {
		t.Errorf("recs[1].DeltaIn = %d, want 0", recs[1].DeltaIn)
	}
}
