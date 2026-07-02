package lane

import (
	"context"
	"path/filepath"
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

func TestLaneStore_CreateAndGet(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	id, err := store.Create(ctx, "interop", "standing", "Plugin interoperability", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty ID")
	}

	l, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if l.Name != "interop" {
		t.Errorf("Name = %q, want %q", l.Name, "interop")
	}
	if l.LaneType != "standing" {
		t.Errorf("LaneType = %q, want %q", l.LaneType, "standing")
	}
	if l.Status != "active" {
		t.Errorf("Status = %q, want %q", l.Status, "active")
	}
	if l.Description != "Plugin interoperability" {
		t.Errorf("Description = %q, want %q", l.Description, "Plugin interoperability")
	}
}

func TestLaneStore_GetByName(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	id, err := store.Create(ctx, "kernel", "standing", "Core kernel work", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	l, err := store.GetByName(ctx, "kernel")
	if err != nil {
		t.Fatalf("GetByName: %v", err)
	}
	if l.ID != id {
		t.Errorf("ID = %q, want %q", l.ID, id)
	}
}

func TestLaneStore_ListActive(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	store.Create(ctx, "interop", "standing", "", "")
	store.Create(ctx, "kernel", "standing", "", "")

	lanes, err := store.List(ctx, "active")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(lanes) != 2 {
		t.Fatalf("List returned %d lanes, want 2", len(lanes))
	}
	// Ordered by name
	if lanes[0].Name != "interop" {
		t.Errorf("lanes[0].Name = %q, want %q", lanes[0].Name, "interop")
	}
	if lanes[1].Name != "kernel" {
		t.Errorf("lanes[1].Name = %q, want %q", lanes[1].Name, "kernel")
	}
}

func TestLaneStore_Close(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	id, _ := store.Create(ctx, "interop", "standing", "", "")

	if err := store.Close(ctx, id); err != nil {
		t.Fatalf("Close: %v", err)
	}

	l, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get after close: %v", err)
	}
	if l.Status != "closed" {
		t.Errorf("Status = %q, want %q", l.Status, "closed")
	}
	if l.ClosedAt == nil {
		t.Error("ClosedAt should be set")
	}

	// Active list should be empty
	active, _ := store.List(ctx, "active")
	if len(active) != 0 {
		t.Errorf("active list has %d lanes, want 0", len(active))
	}

	// Double-close should fail
	if err := store.Close(ctx, id); err == nil {
		t.Error("expected error on double close")
	}
}

func TestLaneStore_DuplicateNameFails(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	_, err := store.Create(ctx, "interop", "standing", "", "")
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}

	_, err = store.Create(ctx, "interop", "arc", "", "")
	if err == nil {
		t.Fatal("expected UNIQUE constraint error on duplicate name")
	}
}

func TestLaneStore_RecordEvent(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	id, _ := store.Create(ctx, "interop", "standing", "", "")

	err := store.RecordEvent(ctx, id, "bead_added", `{"bead_id":"iv-abc1"}`)
	if err != nil {
		t.Fatalf("RecordEvent: %v", err)
	}

	events, err := store.Events(ctx, id)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	// Should have "created" + "bead_added"
	if len(events) != 2 {
		t.Fatalf("Events returned %d, want 2", len(events))
	}
	if events[0].EventType != "created" {
		t.Errorf("events[0].EventType = %q, want %q", events[0].EventType, "created")
	}
	if events[1].EventType != "bead_added" {
		t.Errorf("events[1].EventType = %q, want %q", events[1].EventType, "bead_added")
	}
}

func TestLaneStore_SnapshotMembers(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	id, _ := store.Create(ctx, "interop", "standing", "", "")
	err := store.SnapshotMembers(ctx, id, []string{"iv-rzt0", "iv-sk8t", "iv-sprh"})
	if err != nil {
		t.Fatalf("SnapshotMembers: %v", err)
	}

	members, err := store.GetMembers(ctx, id)
	if err != nil {
		t.Fatalf("GetMembers: %v", err)
	}
	if len(members) != 3 {
		t.Fatalf("GetMembers returned %d, want 3", len(members))
	}

	// Snapshot again with different set — removes stale, adds new
	err = store.SnapshotMembers(ctx, id, []string{"iv-rzt0", "iv-new1"})
	if err != nil {
		t.Fatalf("SnapshotMembers (2nd): %v", err)
	}

	members, err = store.GetMembers(ctx, id)
	if err != nil {
		t.Fatalf("GetMembers (2nd): %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("GetMembers (2nd) returned %d, want 2", len(members))
	}
}

func TestLaneStore_GetLanesForBead(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	id1, _ := store.Create(ctx, "interop", "standing", "", "")
	id2, _ := store.Create(ctx, "kernel", "standing", "", "")

	store.SnapshotMembers(ctx, id1, []string{"iv-abc1", "iv-abc2"})
	store.SnapshotMembers(ctx, id2, []string{"iv-abc1", "iv-abc3"})

	lanes, err := store.GetLanesForBead(ctx, "iv-abc1")
	if err != nil {
		t.Fatalf("GetLanesForBead: %v", err)
	}
	if len(lanes) != 2 {
		t.Fatalf("GetLanesForBead returned %d lanes, want 2", len(lanes))
	}

	lanes, err = store.GetLanesForBead(ctx, "iv-abc3")
	if err != nil {
		t.Fatalf("GetLanesForBead (abc3): %v", err)
	}
	if len(lanes) != 1 {
		t.Fatalf("GetLanesForBead (abc3) returned %d, want 1", len(lanes))
	}
}
