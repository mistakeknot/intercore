package publish

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	dsn := "file:" + path + "?_pragma=journal_mode%3DWAL&_pragma=busy_timeout%3D5000"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })

	store := NewStore(db)
	if err := store.EnsureTable(context.Background()); err != nil {
		t.Fatalf("ensure table: %v", err)
	}
	return db
}

func TestStoreCreateAndGet(t *testing.T) {
	db := setupTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	st := &PublishState{
		PluginName:  "interflux",
		FromVersion: "0.2.29",
		ToVersion:   "0.3.0",
		Phase:       PhaseDiscovery,
		PluginRoot:  "/home/test/interflux",
		MarketRoot:  "/home/test/marketplace",
	}

	if err := store.Create(ctx, st); err != nil {
		t.Fatalf("create: %v", err)
	}

	if st.ID == "" {
		t.Fatal("ID not set after create")
	}

	got, err := store.Get(ctx, st.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if got.PluginName != "interflux" {
		t.Errorf("plugin = %q, want interflux", got.PluginName)
	}
	if got.Phase != PhaseDiscovery {
		t.Errorf("phase = %q, want discovery", got.Phase)
	}
	if got.FromVersion != "0.2.29" {
		t.Errorf("from = %q, want 0.2.29", got.FromVersion)
	}
}

func TestStoreUpdate(t *testing.T) {
	db := setupTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	st := &PublishState{
		PluginName:  "interflux",
		FromVersion: "0.2.29",
		ToVersion:   "0.3.0",
		Phase:       PhaseDiscovery,
		PluginRoot:  "/tmp/test",
		MarketRoot:  "/tmp/mkt",
	}
	store.Create(ctx, st)

	if err := store.Update(ctx, st.ID, PhaseBump, ""); err != nil {
		t.Fatalf("update: %v", err)
	}

	got, _ := store.Get(ctx, st.ID)
	if got.Phase != PhaseBump {
		t.Errorf("phase = %q, want bump", got.Phase)
	}
}

func TestPublishStateIsStale(t *testing.T) {
	const now int64 = 1_000_000_000
	tests := []struct {
		name string
		st   PublishState
		want bool
	}{
		// A recorded error means the attempt failed — stale regardless of age.
		{"failed-recent", PublishState{Error: "git worktree has uncommitted changes", UpdatedAt: now - 5}, true},
		{"failed-old", PublishState{Error: "boom", UpdatedAt: now - 100_000}, true},
		// Clean (no error) records are live until they age past the threshold.
		{"clean-recent", PublishState{Error: "", UpdatedAt: now - 30}, false},
		{"clean-just-under-threshold", PublishState{Error: "", UpdatedAt: now - (staleThresholdSecs - 1)}, false},
		{"clean-at-threshold", PublishState{Error: "", UpdatedAt: now - staleThresholdSecs}, false}, // strictly greater-than
		{"clean-just-over-threshold", PublishState{Error: "", UpdatedAt: now - (staleThresholdSecs + 1)}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.st.IsStale(now); got != tt.want {
				t.Errorf("IsStale(%d) = %v, want %v (UpdatedAt=%d, Error=%q)",
					now, got, tt.want, tt.st.UpdatedAt, tt.st.Error)
			}
		})
	}
}

func TestStoreGetActive(t *testing.T) {
	db := setupTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	// No active publishes
	active, err := store.GetActive(ctx, "interflux")
	if err != nil {
		t.Fatalf("get active: %v", err)
	}
	if active != nil {
		t.Error("expected nil for no active publish")
	}

	// Create an active publish
	st := &PublishState{
		PluginName:  "interflux",
		FromVersion: "0.2.29",
		ToVersion:   "0.3.0",
		Phase:       PhaseBump,
		PluginRoot:  "/tmp/test",
		MarketRoot:  "/tmp/mkt",
	}
	store.Create(ctx, st)

	active, err = store.GetActive(ctx, "interflux")
	if err != nil {
		t.Fatalf("get active: %v", err)
	}
	if active == nil {
		t.Fatal("expected active publish")
	}
	if active.ID != st.ID {
		t.Errorf("active ID = %q, want %q", active.ID, st.ID)
	}

	// Complete it — should no longer be active
	store.Complete(ctx, st.ID)
	active, _ = store.GetActive(ctx, "interflux")
	if active != nil {
		t.Error("completed publish should not be active")
	}
}

func TestStoreList(t *testing.T) {
	db := setupTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	store.Create(ctx, &PublishState{
		PluginName: "a", FromVersion: "0.1.0", ToVersion: "0.2.0",
		Phase: PhaseDiscovery, PluginRoot: "/tmp", MarketRoot: "/tmp",
	})
	store.Create(ctx, &PublishState{
		PluginName: "b", FromVersion: "1.0.0", ToVersion: "1.1.0",
		Phase: PhaseBump, PluginRoot: "/tmp", MarketRoot: "/tmp",
	})

	states, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(states) != 2 {
		t.Errorf("expected 2, got %d", len(states))
	}
}

func TestStoreDelete(t *testing.T) {
	db := setupTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	st := &PublishState{
		PluginName: "test", FromVersion: "0.1.0", ToVersion: "0.2.0",
		Phase: PhaseDiscovery, PluginRoot: "/tmp", MarketRoot: "/tmp",
	}
	store.Create(ctx, st)

	if err := store.Delete(ctx, st.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	_, err := store.Get(ctx, st.ID)
	if err != ErrNoActivePublish {
		t.Errorf("expected ErrNoActivePublish after delete, got %v", err)
	}
}

func TestStoreClearLocksScopedAndAll(t *testing.T) {
	db := setupTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	// Two active locks for different plugins plus one already-done row.
	store.Create(ctx, &PublishState{
		PluginName: "interflux", FromVersion: "0.1.0", ToVersion: "0.2.0",
		Phase: PhaseValidation, PluginRoot: "/tmp", MarketRoot: "/tmp",
	})
	store.Create(ctx, &PublishState{
		PluginName: "clavain", FromVersion: "1.0.0", ToVersion: "1.1.0",
		Phase: PhaseValidation, PluginRoot: "/tmp", MarketRoot: "/tmp",
	})
	doneSt := &PublishState{
		PluginName: "interflux", FromVersion: "0.0.9", ToVersion: "0.1.0",
		Phase: PhaseDiscovery, PluginRoot: "/tmp", MarketRoot: "/tmp",
	}
	store.Create(ctx, doneSt)
	store.Complete(ctx, doneSt.ID)

	// ListActive must report only the two incomplete rows.
	active, err := store.ListActive(ctx)
	if err != nil {
		t.Fatalf("list active: %v", err)
	}
	if len(active) != 2 {
		t.Fatalf("ListActive = %d rows, want 2", len(active))
	}

	// Scoped clear removes only the named plugin's lock.
	n, err := store.ClearLocks(ctx, "interflux")
	if err != nil {
		t.Fatalf("clear locks (interflux): %v", err)
	}
	if n != 1 {
		t.Errorf("ClearLocks(interflux) = %d, want 1", n)
	}
	if a, _ := store.GetActive(ctx, "interflux"); a != nil {
		t.Error("interflux lock should be cleared")
	}
	if a, _ := store.GetActive(ctx, "clavain"); a == nil {
		t.Error("clavain lock should survive scoped clear")
	}

	// Global clear sweeps the rest.
	n, err = store.ClearLocks(ctx, "")
	if err != nil {
		t.Fatalf("clear locks (all): %v", err)
	}
	if n != 1 {
		t.Errorf("ClearLocks(all) = %d, want 1", n)
	}
	active, _ = store.ListActive(ctx)
	if len(active) != 0 {
		t.Errorf("ListActive after global clear = %d, want 0", len(active))
	}

	// The completed row must remain untouched by both clears.
	if got, err := store.Get(ctx, doneSt.ID); err != nil || got.Phase != PhaseDone {
		t.Errorf("completed row should survive ClearLocks (err=%v)", err)
	}
}

func TestPhaseIndex(t *testing.T) {
	if PhaseIndex(PhaseDiscovery) != 0 {
		t.Error("discovery should be 0")
	}
	if PhaseIndex(PhaseDone) != 7 {
		t.Errorf("done should be 7, got %d", PhaseIndex(PhaseDone))
	}
	if PhaseIndex("bogus") != -1 {
		t.Error("bogus should be -1")
	}
}
