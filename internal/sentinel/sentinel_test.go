package sentinel

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	dsn := "file:" + path + "?_pragma=journal_mode%3DWAL&_pragma=busy_timeout%3D5000"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })

	_, err = db.Exec(`
		CREATE TABLE sentinels (
			name TEXT NOT NULL,
			scope_id TEXT NOT NULL,
			last_fired INTEGER NOT NULL DEFAULT (unixepoch()),
			PRIMARY KEY (name, scope_id)
		)`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	return db
}

func TestSentinelCheck(t *testing.T) {
	tests := []struct {
		name        string
		sentinel    string
		scopeID     string
		interval    int
		setupFired  int64 // 0 = let Check create it fresh
		wantAllowed bool
	}{
		{"fresh sentinel fires", "stop", "s1", 0, 0, true},
		{"interval=5 fresh fires", "rate", "s1", 5, 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := setupTestDB(t)
			store := New(db)
			ctx := context.Background()

			if tt.setupFired > 0 {
				_, err := db.Exec("INSERT INTO sentinels (name, scope_id, last_fired) VALUES (?, ?, ?)",
					tt.sentinel, tt.scopeID, tt.setupFired)
				if err != nil {
					t.Fatal(err)
				}
			}

			allowed, err := store.Check(ctx, tt.sentinel, tt.scopeID, tt.interval)
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			if allowed != tt.wantAllowed {
				t.Errorf("allowed = %v, want %v", allowed, tt.wantAllowed)
			}
		})
	}
}

func TestSentinelCheck_Interval0_OnceOnly(t *testing.T) {
	db := setupTestDB(t)
	store := New(db)
	ctx := context.Background()

	// First call: should be allowed
	allowed, err := store.Check(ctx, "stop", "s1", 0)
	if err != nil {
		t.Fatalf("Check 1: %v", err)
	}
	if !allowed {
		t.Error("first check should be allowed")
	}

	// Second call: should be throttled
	allowed, err = store.Check(ctx, "stop", "s1", 0)
	if err != nil {
		t.Fatalf("Check 2: %v", err)
	}
	if allowed {
		t.Error("second check should be throttled")
	}

	// Third call: still throttled
	allowed, err = store.Check(ctx, "stop", "s1", 0)
	if err != nil {
		t.Fatalf("Check 3: %v", err)
	}
	if allowed {
		t.Error("third check should be throttled")
	}
}

func TestSentinelCheck_IntervalExpiry(t *testing.T) {
	db := setupTestDB(t)
	store := New(db)
	ctx := context.Background()

	// First call: allowed
	allowed, err := store.Check(ctx, "rate", "s1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !allowed {
		t.Error("first check should be allowed")
	}

	// Immediate second call: throttled
	allowed, err = store.Check(ctx, "rate", "s1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if allowed {
		t.Error("immediate second check should be throttled")
	}

	// Wait for interval to expire
	time.Sleep(1100 * time.Millisecond)

	// Third call: allowed again
	allowed, err = store.Check(ctx, "rate", "s1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !allowed {
		t.Error("check after interval should be allowed")
	}
}

func TestSentinelCheck_Concurrent(t *testing.T) {
	db := setupTestDB(t)
	store := New(db)
	ctx := context.Background()

	var wg sync.WaitGroup
	var allowedCount atomic.Int32

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			allowed, err := store.Check(ctx, "race", "s1", 0)
			if err != nil {
				t.Errorf("Check: %v", err)
				return
			}
			if allowed {
				allowedCount.Add(1)
			}
		}()
	}

	wg.Wait()

	if count := allowedCount.Load(); count != 1 {
		t.Errorf("expected exactly 1 allowed, got %d", count)
	}
}

func TestSentinelReset(t *testing.T) {
	db := setupTestDB(t)
	store := New(db)
	ctx := context.Background()

	// Fire sentinel
	allowed, _ := store.Check(ctx, "stop", "s1", 0)
	if !allowed {
		t.Fatal("expected allowed")
	}

	// Reset
	if err := store.Reset(ctx, "stop", "s1"); err != nil {
		t.Fatal(err)
	}

	// Should fire again
	allowed, _ = store.Check(ctx, "stop", "s1", 0)
	if !allowed {
		t.Error("expected allowed after reset")
	}
}

func TestSentinelList(t *testing.T) {
	db := setupTestDB(t)
	store := New(db)
	ctx := context.Background()

	store.Check(ctx, "a", "s1", 0)
	store.Check(ctx, "b", "s2", 0)

	sentinels, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sentinels) != 2 {
		t.Errorf("expected 2 sentinels, got %d", len(sentinels))
	}
}

func TestSentinelPrune(t *testing.T) {
	db := setupTestDB(t)
	store := New(db)
	ctx := context.Background()

	// Create a sentinel and back-date it
	_, err := db.Exec("INSERT INTO sentinels (name, scope_id, last_fired) VALUES (?, ?, ?)",
		"old", "s1", time.Now().Unix()-100)
	if err != nil {
		t.Fatal(err)
	}

	// Prune sentinels older than 10 seconds
	count, err := store.Prune(ctx, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("expected 1 pruned, got %d", count)
	}
}
