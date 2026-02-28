package observation

import (
	"context"
	"testing"
)

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
