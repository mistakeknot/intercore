package observation

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mistakeknot/intercore/internal/phase"
	pkgphase "github.com/mistakeknot/intercore/pkg/phase"
)

// runToSummary must annotate every DefaultChain lifecycle phase with the
// OODARC role pkg/phase defines for it. This pins the consumer wiring to the
// single source of truth: if pkg/phase's map and this projection ever drift,
// this test fails rather than silently shipping a stale or wrong label.
func TestRunToSummaryAnnotatesOODARCRole(t *testing.T) {
	for _, p := range pkgphase.DefaultChain {
		want, ok := pkgphase.OODARCRole(p)
		if !ok {
			// Guarded by pkg/phase's own TestEveryDefaultChainPhaseHasRole, but
			// assert here too so a regression surfaces at the consumer boundary.
			t.Fatalf("pkg/phase.OODARCRole(%q) returned no role for a DefaultChain phase", p)
		}
		got := runToSummary(&phase.Run{ID: "r-" + p, Phase: p})
		if got.OODARCRole != want {
			t.Errorf("runToSummary phase=%q OODARCRole = %q, want %q", p, got.OODARCRole, want)
		}
	}
}

// An unknown lifecycle phase must leave the role empty — never a defaulted
// "observe". Empty + omitempty means the field disappears from JSON, so a
// consumer reads absence as "no known role" rather than a wrong leg.
func TestRunToSummaryUnknownPhaseHasEmptyRole(t *testing.T) {
	got := runToSummary(&phase.Run{ID: "r-x", Phase: "not-a-phase"})
	if got.OODARCRole != "" {
		t.Errorf("unknown phase OODARCRole = %q, want empty", got.OODARCRole)
	}

	b, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "oodarc_role") {
		t.Errorf("empty role must be omitted from JSON; got %s", b)
	}
}

// End-to-end through Collect: a freshly created run sits in brainstorm, whose
// role is observe, and the snapshot JSON carries oodarc_role for it.
func TestCollectSurfacesOODARCRole(t *testing.T) {
	c, _ := testCollector(t)
	ctx := context.Background()

	pStore := c.phases.(*phase.Store)
	if _, err := pStore.Create(ctx, &phase.Run{
		ProjectDir: "/tmp/oodarc-role",
		Goal:       "surface the role",
		Complexity: 2,
	}); err != nil {
		t.Fatalf("phase.Create: %v", err)
	}

	snap, err := c.Collect(ctx, CollectOptions{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(snap.Runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(snap.Runs))
	}
	run := snap.Runs[0]
	if run.Phase != phase.PhaseBrainstorm {
		t.Fatalf("seeded run Phase = %q, want %q", run.Phase, phase.PhaseBrainstorm)
	}
	if run.OODARCRole != pkgphase.RoleObserve {
		t.Errorf("brainstorm OODARCRole = %q, want %q", run.OODARCRole, pkgphase.RoleObserve)
	}

	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	if !strings.Contains(string(b), `"oodarc_role":"observe"`) {
		t.Errorf("snapshot JSON missing oodarc_role for brainstorm run: %s", b)
	}
}
