package lane

import (
	"context"
	"testing"
)

func TestLaneVelocity_RelativeStarvation(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()
	v := NewVelocityCalculator(store)

	// Create two lanes
	interopID, _ := store.Create(ctx, "interop", "standing", "", "")
	kernelID, _ := store.Create(ctx, "kernel", "standing", "", "")

	// interop: 5 open P2 beads, 0 closed
	store.SnapshotMembers(ctx, interopID, []string{"iv-a1", "iv-a2", "iv-a3", "iv-a4", "iv-a5"})

	// kernel: 2 open P2 beads, 3 closed recently
	store.SnapshotMembers(ctx, kernelID, []string{"iv-b1", "iv-b2"})

	beadStatuses := map[string]*BeadStatus{
		"iv-a1": {BeadID: "iv-a1", Priority: 2, IsClosed: false},
		"iv-a2": {BeadID: "iv-a2", Priority: 2, IsClosed: false},
		"iv-a3": {BeadID: "iv-a3", Priority: 2, IsClosed: false},
		"iv-a4": {BeadID: "iv-a4", Priority: 1, IsClosed: false}, // P1 = higher weight
		"iv-a5": {BeadID: "iv-a5", Priority: 0, IsClosed: false}, // P0 = highest weight
		"iv-b1": {BeadID: "iv-b1", Priority: 2, IsClosed: false},
		"iv-b2": {BeadID: "iv-b2", Priority: 2, IsClosed: false},
	}

	scores, err := v.ComputeStarvation(ctx, beadStatuses, 7)
	if err != nil {
		t.Fatalf("ComputeStarvation: %v", err)
	}

	interopScore := scores["interop"]
	kernelScore := scores["kernel"]

	if interopScore == nil || kernelScore == nil {
		t.Fatal("expected scores for both lanes")
	}

	// interop: 5 open beads (3+3+3+4+5 = 18 weighted), 0 throughput → 18/0.1 = 180
	// kernel: 2 open beads (3+3 = 6 weighted), 0 throughput → 6/0.1 = 60
	// interop should be more starved
	if interopScore.Starvation <= kernelScore.Starvation {
		t.Errorf("interop starvation (%v) should be > kernel starvation (%v)",
			interopScore.Starvation, kernelScore.Starvation)
	}

	if interopScore.OpenBeads != 5 {
		t.Errorf("interop OpenBeads = %d, want 5", interopScore.OpenBeads)
	}
	if kernelScore.OpenBeads != 2 {
		t.Errorf("kernel OpenBeads = %d, want 2", kernelScore.OpenBeads)
	}
}

func TestLaneVelocity_ThroughputReducesStarvation(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()
	v := NewVelocityCalculator(store)

	id, _ := store.Create(ctx, "active-lane", "standing", "", "")
	store.SnapshotMembers(ctx, id, []string{"iv-c1", "iv-c2", "iv-c3"})

	// All open, some recently closed (via bead status)
	beadStatuses := map[string]*BeadStatus{
		"iv-c1": {BeadID: "iv-c1", Priority: 2, IsClosed: false},
		"iv-c2": {BeadID: "iv-c2", Priority: 2, IsClosed: false},
		"iv-c3": {BeadID: "iv-c3", Priority: 2, IsClosed: false},
	}

	scores, err := v.ComputeStarvation(ctx, beadStatuses, 7)
	if err != nil {
		t.Fatalf("ComputeStarvation: %v", err)
	}

	// 3 open P2 beads, 0 throughput → 9/0.1 = 90
	if scores["active-lane"].Starvation != 90.0 {
		t.Errorf("starvation = %v, want 90.0", scores["active-lane"].Starvation)
	}
}

func TestLaneVelocity_SortedByStarvation(t *testing.T) {
	scores := map[string]*VelocityScore{
		"low":    {LaneName: "low", Starvation: 1.0},
		"high":   {LaneName: "high", Starvation: 10.0},
		"medium": {LaneName: "medium", Starvation: 5.0},
	}

	sorted := SortedByStarvation(scores)
	if len(sorted) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(sorted))
	}
	if sorted[0].LaneName != "high" {
		t.Errorf("sorted[0] = %s, want high", sorted[0].LaneName)
	}
	if sorted[1].LaneName != "medium" {
		t.Errorf("sorted[1] = %s, want medium", sorted[1].LaneName)
	}
	if sorted[2].LaneName != "low" {
		t.Errorf("sorted[2] = %s, want low", sorted[2].LaneName)
	}
}

func TestLaneVelocity_FromDB(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()
	v := NewVelocityCalculator(store)

	id, _ := store.Create(ctx, "db-lane", "standing", "", "")
	store.SnapshotMembers(ctx, id, []string{"iv-d1", "iv-d2", "iv-d3"})

	scores, err := v.ComputeStarvationFromDB(ctx, 7)
	if err != nil {
		t.Fatalf("ComputeStarvationFromDB: %v", err)
	}

	s := scores["db-lane"]
	if s == nil {
		t.Fatal("expected score for db-lane")
	}
	if s.OpenBeads != 3 {
		t.Errorf("OpenBeads = %d, want 3", s.OpenBeads)
	}
	// 3 members * 3 (P2 weight) / 0.1 (no throughput) = 90
	if s.Starvation != 90.0 {
		t.Errorf("Starvation = %v, want 90.0", s.Starvation)
	}
}
