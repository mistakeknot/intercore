package dispatch

import (
	"context"
	"math"
	"testing"
)

func TestGetConflictTelemetry_Empty(t *testing.T) {
	dstore := testStore(t)
	ts := NewTelemetryStore(dstore.db)
	ctx := context.Background()

	tel, err := ts.GetConflictTelemetry(ctx, "")
	if err != nil {
		t.Fatalf("GetConflictTelemetry: %v", err)
	}
	if tel.TotalDispatches != 0 {
		t.Errorf("TotalDispatches = %d, want 0", tel.TotalDispatches)
	}
}

func TestGetConflictTelemetry_WithData(t *testing.T) {
	dstore := testStore(t)
	ts := NewTelemetryStore(dstore.db)
	ctx := context.Background()
	scope := "run-1"

	// Create 3 dispatches in scope
	for i := 0; i < 3; i++ {
		d := &Dispatch{
			AgentType:  "codex",
			ProjectDir: "/tmp/proj",
			ScopeID:    &scope,
		}
		dstore.Create(ctx, d)
	}

	// Create 1 dispatch outside scope
	d := &Dispatch{
		AgentType:  "codex",
		ProjectDir: "/tmp/proj",
	}
	dstore.Create(ctx, d)

	tel, err := ts.GetConflictTelemetry(ctx, scope)
	if err != nil {
		t.Fatalf("GetConflictTelemetry: %v", err)
	}
	if tel.TotalDispatches != 3 {
		t.Errorf("TotalDispatches = %d, want 3", tel.TotalDispatches)
	}
	if tel.WithConflicts != 0 {
		t.Errorf("WithConflicts = %d, want 0", tel.WithConflicts)
	}
}

func TestRecordConflict(t *testing.T) {
	dstore := testStore(t)
	ts := NewTelemetryStore(dstore.db)
	ctx := context.Background()
	scope := "run-1"

	d := &Dispatch{
		AgentType:  "codex",
		ProjectDir: "/tmp/proj",
		ScopeID:    &scope,
	}
	id, _ := dstore.Create(ctx, d)

	// Record a write-write conflict
	if err := ts.RecordConflict(ctx, id, ConflictTypeWriteWrite, "concurrent edit on model.go"); err != nil {
		t.Fatalf("RecordConflict: %v", err)
	}

	// Check the dispatch was updated
	got, _ := dstore.Get(ctx, id)
	if got.ConflictType == nil || *got.ConflictType != ConflictTypeWriteWrite {
		t.Errorf("ConflictType = %v, want %q", got.ConflictType, ConflictTypeWriteWrite)
	}
	if got.QuarantineReason == nil || *got.QuarantineReason != "concurrent edit on model.go" {
		t.Errorf("QuarantineReason = %v, want %q", got.QuarantineReason, "concurrent edit on model.go")
	}
	if got.RetryCount != 1 {
		t.Errorf("RetryCount = %d, want 1", got.RetryCount)
	}

	// Record another conflict — retry count should increment
	ts.RecordConflict(ctx, id, ConflictTypeWriteWrite, "")
	got, _ = dstore.Get(ctx, id)
	if got.RetryCount != 2 {
		t.Errorf("RetryCount = %d, want 2", got.RetryCount)
	}
}

func TestGetConflictTelemetry_WithConflicts(t *testing.T) {
	dstore := testStore(t)
	ts := NewTelemetryStore(dstore.db)
	ctx := context.Background()
	scope := "run-1"

	// Create 4 dispatches, 2 with conflicts
	ids := make([]string, 4)
	for i := 0; i < 4; i++ {
		d := &Dispatch{AgentType: "codex", ProjectDir: "/tmp/proj", ScopeID: &scope}
		ids[i], _ = dstore.Create(ctx, d)
	}

	// Mark 2 as completed
	dstore.UpdateStatus(ctx, ids[0], StatusCompleted, UpdateFields{"completed_at": int64(100)})
	dstore.UpdateStatus(ctx, ids[1], StatusCompleted, UpdateFields{"completed_at": int64(101)})

	// Record conflicts on 2 of the dispatches
	ts.RecordConflict(ctx, ids[0], ConflictTypeWriteWrite, "")
	ts.RecordConflict(ctx, ids[2], ConflictTypeWriteWrite, "needs rebase")

	tel, err := ts.GetConflictTelemetry(ctx, scope)
	if err != nil {
		t.Fatalf("GetConflictTelemetry: %v", err)
	}
	if tel.TotalDispatches != 4 {
		t.Errorf("TotalDispatches = %d, want 4", tel.TotalDispatches)
	}
	if tel.WithConflicts != 2 {
		t.Errorf("WithConflicts = %d, want 2", tel.WithConflicts)
	}
	if tel.WriteWriteCount != 2 {
		t.Errorf("WriteWriteCount = %d, want 2", tel.WriteWriteCount)
	}
	if tel.TotalRetries != 2 {
		t.Errorf("TotalRetries = %d, want 2", tel.TotalRetries)
	}
	if tel.QuarantinedCount != 1 {
		t.Errorf("QuarantinedCount = %d, want 1", tel.QuarantinedCount)
	}

	expectedRate := 2.0 / 4.0
	if math.Abs(tel.ConflictRate-expectedRate) > 0.001 {
		t.Errorf("ConflictRate = %f, want %f", tel.ConflictRate, expectedRate)
	}
}

func TestGetConflictsByType(t *testing.T) {
	dstore := testStore(t)
	ts := NewTelemetryStore(dstore.db)
	ctx := context.Background()

	// Create dispatches with different conflict types
	ids := make([]string, 3)
	for i := 0; i < 3; i++ {
		d := &Dispatch{AgentType: "codex", ProjectDir: "/tmp/proj"}
		ids[i], _ = dstore.Create(ctx, d)
	}

	ts.RecordConflict(ctx, ids[0], ConflictTypeWriteWrite, "")
	ts.RecordConflict(ctx, ids[1], ConflictTypeWriteWrite, "")
	ts.RecordConflict(ctx, ids[2], "semantic", "")

	byType, err := ts.GetConflictsByType(ctx, "")
	if err != nil {
		t.Fatalf("GetConflictsByType: %v", err)
	}
	if len(byType) != 2 {
		t.Fatalf("expected 2 conflict types, got %d", len(byType))
	}
	// write_write should be first (count=2)
	if byType[0].ConflictType != ConflictTypeWriteWrite || byType[0].Count != 2 {
		t.Errorf("byType[0] = %+v, want write_write/2", byType[0])
	}
}

func TestIncrementRetry(t *testing.T) {
	dstore := testStore(t)
	ts := NewTelemetryStore(dstore.db)
	ctx := context.Background()

	d := &Dispatch{AgentType: "codex", ProjectDir: "/tmp/proj"}
	id, _ := dstore.Create(ctx, d)

	for i := 0; i < 3; i++ {
		if err := ts.IncrementRetry(ctx, id); err != nil {
			t.Fatalf("IncrementRetry: %v", err)
		}
	}

	got, _ := dstore.Get(ctx, id)
	if got.RetryCount != 3 {
		t.Errorf("RetryCount = %d, want 3", got.RetryCount)
	}
}
