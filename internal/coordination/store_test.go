package coordination

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func setupTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode%3DWAL&_pragma=busy_timeout%3D5000")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })

	// Create tables
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS coordination_locks (
		id TEXT PRIMARY KEY,
		type TEXT NOT NULL CHECK(type IN ('file_reservation','named_lock','write_set')),
		owner TEXT NOT NULL,
		scope TEXT NOT NULL,
		pattern TEXT NOT NULL,
		exclusive INTEGER NOT NULL DEFAULT 1,
		reason TEXT,
		ttl_seconds INTEGER,
		created_at INTEGER NOT NULL,
		expires_at INTEGER,
		released_at INTEGER,
		dispatch_id TEXT,
		run_id TEXT)`)
	if err != nil {
		t.Fatal(err)
	}

	return NewStore(db)
}

func TestReserve_Success(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	result, err := s.Reserve(ctx, Lock{
		Type:       TypeFileReservation,
		Owner:      "agent-1",
		Scope:      "/tmp/project",
		Pattern:    "*.go",
		Exclusive:  true,
		TTLSeconds: 60,
	})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if result.Lock == nil {
		t.Fatal("expected lock, got nil")
	}
	if result.Lock.ID == "" {
		t.Error("expected non-empty ID")
	}
	if len(result.Lock.ID) != idLen {
		t.Errorf("ID length = %d, want %d", len(result.Lock.ID), idLen)
	}
	if result.Lock.ExpiresAt == nil {
		t.Error("expected non-nil ExpiresAt")
	}
	if result.Conflict != nil {
		t.Error("expected no conflict")
	}
}

func TestReserve_Conflict(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	// First reserve succeeds
	_, err := s.Reserve(ctx, Lock{
		Type:       TypeFileReservation,
		Owner:      "agent-1",
		Scope:      "/tmp/project",
		Pattern:    "*.go",
		Exclusive:  true,
		TTLSeconds: 60,
	})
	if err != nil {
		t.Fatalf("Reserve 1: %v", err)
	}

	// Second reserve by different owner on overlapping pattern conflicts
	result, err := s.Reserve(ctx, Lock{
		Type:       TypeFileReservation,
		Owner:      "agent-2",
		Scope:      "/tmp/project",
		Pattern:    "main.go",
		Exclusive:  true,
		TTLSeconds: 60,
	})
	if err != nil {
		t.Fatalf("Reserve 2: %v", err)
	}
	if result.Lock != nil {
		t.Error("expected no lock on conflict")
	}
	if result.Conflict == nil {
		t.Fatal("expected conflict info")
	}
	if result.Conflict.BlockerOwner != "agent-1" {
		t.Errorf("BlockerOwner = %q, want agent-1", result.Conflict.BlockerOwner)
	}
}

func TestReserve_SharedSharedAllowed(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	_, err := s.Reserve(ctx, Lock{
		Type:       TypeFileReservation,
		Owner:      "agent-1",
		Scope:      "/tmp/project",
		Pattern:    "*.go",
		Exclusive:  false,
		TTLSeconds: 60,
	})
	if err != nil {
		t.Fatalf("Reserve 1: %v", err)
	}

	result, err := s.Reserve(ctx, Lock{
		Type:       TypeFileReservation,
		Owner:      "agent-2",
		Scope:      "/tmp/project",
		Pattern:    "main.go",
		Exclusive:  false,
		TTLSeconds: 60,
	})
	if err != nil {
		t.Fatalf("Reserve 2: %v", err)
	}
	if result.Lock == nil {
		t.Error("expected lock (shared+shared should not conflict)")
	}
}

func TestReserve_NoConflictDifferentScope(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	_, err := s.Reserve(ctx, Lock{
		Type:       TypeFileReservation,
		Owner:      "agent-1",
		Scope:      "/tmp/project-a",
		Pattern:    "*.go",
		Exclusive:  true,
		TTLSeconds: 60,
	})
	if err != nil {
		t.Fatalf("Reserve 1: %v", err)
	}

	result, err := s.Reserve(ctx, Lock{
		Type:       TypeFileReservation,
		Owner:      "agent-2",
		Scope:      "/tmp/project-b",
		Pattern:    "*.go",
		Exclusive:  true,
		TTLSeconds: 60,
	})
	if err != nil {
		t.Fatalf("Reserve 2: %v", err)
	}
	if result.Lock == nil {
		t.Error("expected lock (different scopes should not conflict)")
	}
}

func TestReserve_InvalidPattern(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	// Overly complex pattern
	complex := "?/?/?/?/?/?/?/?/?/?/?/?/?/?/?/?/?/?/?/?/?/?/?/?/?/?/?/?/?/?"
	_, err := s.Reserve(ctx, Lock{
		Type:       TypeFileReservation,
		Owner:      "agent-1",
		Scope:      "/tmp/project",
		Pattern:    complex,
		Exclusive:  true,
		TTLSeconds: 60,
	})
	if err == nil {
		t.Error("expected error for overly complex pattern")
	}
}

func TestRelease_ByID(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	result, err := s.Reserve(ctx, Lock{
		Type:       TypeFileReservation,
		Owner:      "agent-1",
		Scope:      "/tmp/project",
		Pattern:    "*.go",
		Exclusive:  true,
		TTLSeconds: 60,
	})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	n, err := s.Release(ctx, result.Lock.ID, "", "")
	if err != nil {
		t.Fatalf("Release: %v", err)
	}
	if n != 1 {
		t.Errorf("Release rows affected = %d, want 1", n)
	}

	// Verify gone from active list
	locks, err := s.List(ctx, ListFilter{Active: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(locks) != 0 {
		t.Errorf("expected 0 active locks, got %d", len(locks))
	}
}

func TestRelease_ByOwnerScope(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		_, err := s.Reserve(ctx, Lock{
			Type:       TypeFileReservation,
			Owner:      "agent-1",
			Scope:      "/tmp/project",
			Pattern:    fmt.Sprintf("file%d.go", i),
			Exclusive:  true,
			TTLSeconds: 60,
		})
		if err != nil {
			t.Fatalf("Reserve %d: %v", i, err)
		}
	}

	n, err := s.Release(ctx, "", "agent-1", "/tmp/project")
	if err != nil {
		t.Fatalf("Release: %v", err)
	}
	if n != 3 {
		t.Errorf("Release rows affected = %d, want 3", n)
	}
}

func TestCheck_Overlap(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	_, err := s.Reserve(ctx, Lock{
		Type:       TypeFileReservation,
		Owner:      "agent-1",
		Scope:      "/tmp/project",
		Pattern:    "*.go",
		Exclusive:  true,
		TTLSeconds: 60,
	})
	if err != nil {
		t.Fatal(err)
	}

	conflicts, err := s.Check(ctx, "/tmp/project", "main.go", "")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(conflicts) != 1 {
		t.Errorf("expected 1 conflict, got %d", len(conflicts))
	}
}

func TestCheck_ExcludeOwner(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	_, err := s.Reserve(ctx, Lock{
		Type:       TypeFileReservation,
		Owner:      "agent-1",
		Scope:      "/tmp/project",
		Pattern:    "*.go",
		Exclusive:  true,
		TTLSeconds: 60,
	})
	if err != nil {
		t.Fatal(err)
	}

	conflicts, err := s.Check(ctx, "/tmp/project", "main.go", "agent-1")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(conflicts) != 0 {
		t.Errorf("expected 0 conflicts (excluded owner), got %d", len(conflicts))
	}
}

func TestList_Filters(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	_, _ = s.Reserve(ctx, Lock{Type: TypeFileReservation, Owner: "a1", Scope: "/p", Pattern: "a.go", Exclusive: true, TTLSeconds: 60})
	_, _ = s.Reserve(ctx, Lock{Type: TypeNamedLock, Owner: "a2", Scope: "/p", Pattern: "build", Exclusive: true, TTLSeconds: 60})

	// All
	locks, err := s.List(ctx, ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(locks) != 2 {
		t.Errorf("all: got %d, want 2", len(locks))
	}

	// By type
	locks, err = s.List(ctx, ListFilter{Type: TypeNamedLock})
	if err != nil {
		t.Fatal(err)
	}
	if len(locks) != 1 {
		t.Errorf("by type: got %d, want 1", len(locks))
	}

	// By owner
	locks, err = s.List(ctx, ListFilter{Owner: "a1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(locks) != 1 {
		t.Errorf("by owner: got %d, want 1", len(locks))
	}
}

func TestSweep_Expired(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	// Create a lock with TTL=1
	_, err := s.Reserve(ctx, Lock{
		Type:       TypeFileReservation,
		Owner:      "agent-1",
		Scope:      "/tmp/project",
		Pattern:    "*.go",
		Exclusive:  true,
		TTLSeconds: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Wait for expiry
	time.Sleep(2 * time.Second)

	result, err := s.Sweep(ctx, 0, false)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if result.Expired != 1 {
		t.Errorf("Expired = %d, want 1", result.Expired)
	}

	// Verify lock is released
	locks, err := s.List(ctx, ListFilter{Active: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(locks) != 0 {
		t.Errorf("expected 0 active locks after sweep, got %d", len(locks))
	}
}

func TestSweep_DryRun(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	_, err := s.Reserve(ctx, Lock{
		Type:       TypeFileReservation,
		Owner:      "agent-1",
		Scope:      "/tmp/project",
		Pattern:    "*.go",
		Exclusive:  true,
		TTLSeconds: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(2 * time.Second)

	result, err := s.Sweep(ctx, 0, true)
	if err != nil {
		t.Fatalf("Sweep dry-run: %v", err)
	}
	if result.Expired != 1 {
		t.Errorf("DryRun Expired = %d, want 1", result.Expired)
	}

	// Lock should still be active (not released)
	locks, err := s.List(ctx, ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	activeCount := 0
	for _, l := range locks {
		if l.ReleasedAt == nil {
			activeCount++
		}
	}
	if activeCount != 1 {
		t.Errorf("expected 1 unreleased lock after dry-run, got %d", activeCount)
	}
}

func TestTransfer_Success(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		_, err := s.Reserve(ctx, Lock{
			Type:       TypeFileReservation,
			Owner:      "agent-old",
			Scope:      "/tmp/project",
			Pattern:    fmt.Sprintf("file%d.go", i),
			Exclusive:  true,
			TTLSeconds: 60,
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	n, err := s.Transfer(ctx, "agent-old", "agent-new", "/tmp/project", false)
	if err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	if n != 3 {
		t.Errorf("Transfer affected = %d, want 3", n)
	}

	// Verify new owner
	locks, err := s.List(ctx, ListFilter{Owner: "agent-new", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(locks) != 3 {
		t.Errorf("agent-new locks = %d, want 3", len(locks))
	}
}

func TestTransfer_ConflictBlocked(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	// agent-old holds *.go, agent-new holds *.rs — non-overlapping so both succeed
	_, _ = s.Reserve(ctx, Lock{Type: TypeFileReservation, Owner: "agent-old", Scope: "/p", Pattern: "*.go", Exclusive: true, TTLSeconds: 60})
	// agent-new needs a non-overlapping pattern so Reserve succeeds, but then Transfer creates overlap
	_, _ = s.Reserve(ctx, Lock{Type: TypeFileReservation, Owner: "agent-new", Scope: "/p", Pattern: "*.rs", Exclusive: true, TTLSeconds: 60})
	// Also give agent-new a lock that would overlap with agent-old's *.go AFTER transfer
	// We need agent-new to hold something that overlaps with agent-old's patterns.
	// Actually, *.rs doesn't overlap with *.go. But agent-old's *.go transferred to agent-new
	// doesn't conflict with agent-new's *.rs. We need agent-new to have exclusive *.go.
	// But that would fail on Reserve too. So use a different scope trick:
	// Give agent-new a lock on a pattern that overlaps with agent-old's.
	// Since they can't both have it at the same time via Reserve (conflict detection),
	// we insert directly into DB for the test setup.
	s.db.ExecContext(ctx, `INSERT INTO coordination_locks
		(id, type, owner, scope, pattern, exclusive, ttl_seconds, created_at, expires_at)
		VALUES ('manual1', 'file_reservation', 'agent-new', '/p', 'main.go', 1, 60, ?, ?)`,
		time.Now().Unix(), time.Now().Unix()+60)

	_, err := s.Transfer(ctx, "agent-old", "agent-new", "/p", false)
	if err == nil {
		t.Error("expected transfer conflict error")
	}
}

func TestTransfer_ForceIgnoresConflict(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	_, _ = s.Reserve(ctx, Lock{Type: TypeFileReservation, Owner: "agent-old", Scope: "/p", Pattern: "*.go", Exclusive: true, TTLSeconds: 60})
	_, _ = s.Reserve(ctx, Lock{Type: TypeFileReservation, Owner: "agent-new", Scope: "/p", Pattern: "main.go", Exclusive: true, TTLSeconds: 60})

	n, err := s.Transfer(ctx, "agent-old", "agent-new", "/p", true)
	if err != nil {
		t.Fatalf("Transfer --force: %v", err)
	}
	if n != 1 {
		t.Errorf("Transfer affected = %d, want 1", n)
	}
}

func TestEventEmission(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	var events []string
	s.SetEventFunc(func(_ context.Context, eventType, _, _, _, _, _, _ string) error {
		events = append(events, eventType)
		return nil
	})

	// Reserve → acquired
	result, err := s.Reserve(ctx, Lock{
		Type:       TypeFileReservation,
		Owner:      "agent-1",
		Scope:      "/tmp/project",
		Pattern:    "*.go",
		Exclusive:  true,
		TTLSeconds: 60,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Reserve conflict → conflict
	_, err = s.Reserve(ctx, Lock{
		Type:       TypeFileReservation,
		Owner:      "agent-2",
		Scope:      "/tmp/project",
		Pattern:    "main.go",
		Exclusive:  true,
		TTLSeconds: 60,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Release → released
	_, err = s.Release(ctx, result.Lock.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"coordination.acquired", "coordination.conflict", "coordination.released"}
	if len(events) != len(want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Errorf("events[%d] = %q, want %q", i, events[i], want[i])
		}
	}
}
