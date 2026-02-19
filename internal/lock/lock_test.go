package lock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func setupTestManager(t *testing.T) *Manager {
	t.Helper()
	dir := t.TempDir()
	m := NewManager(filepath.Join(dir, "locks"))
	m.StaleAge = 500 * time.Millisecond // Short for testing.
	return m
}

func TestAcquireRelease(t *testing.T) {
	m := setupTestManager(t)
	ctx := context.Background()

	err := m.Acquire(ctx, "test", "scope1", "123:host", time.Second)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// Verify lock dir exists.
	ld, _ := m.lockDir("test", "scope1")
	if _, err := os.Stat(ld); err != nil {
		t.Fatalf("lock dir missing: %v", err)
	}

	// Verify owner.json exists and is correct.
	meta, err := readOwnerFile(ownerFilePath(ld))
	if err != nil {
		t.Fatalf("read owner: %v", err)
	}
	if meta.Owner != "123:host" {
		t.Errorf("owner = %q, want %q", meta.Owner, "123:host")
	}
	if meta.PID != 123 {
		t.Errorf("pid = %d, want 123", meta.PID)
	}

	// Release.
	err = m.Release(ctx, "test", "scope1", "123:host")
	if err != nil {
		t.Fatalf("release: %v", err)
	}

	// Verify lock dir removed.
	if _, err := os.Stat(ld); !os.IsNotExist(err) {
		t.Errorf("lock dir still exists after release")
	}
}

func TestAcquireContention(t *testing.T) {
	m := setupTestManager(t)
	m.StaleAge = 30 * time.Second // Long stale age — lock should NOT be broken during test.
	ctx := context.Background()

	// First acquire succeeds.
	err := m.Acquire(ctx, "mutex", "s1", "a:host", time.Second)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	// Second acquire times out (lock is fresh, not stale).
	err = m.Acquire(ctx, "mutex", "s1", "b:host", 300*time.Millisecond)
	if !errors.Is(err, ErrTimeout) {
		t.Errorf("second acquire = %v, want ErrTimeout", err)
	}

	// Release first, then second should succeed.
	m.Release(ctx, "mutex", "s1", "a:host")

	err = m.Acquire(ctx, "mutex", "s1", "b:host", time.Second)
	if err != nil {
		t.Fatalf("second acquire after release: %v", err)
	}
	m.Release(ctx, "mutex", "s1", "b:host")
}

func TestStaleBreaking(t *testing.T) {
	m := setupTestManager(t)
	ctx := context.Background()

	// Acquire a lock.
	err := m.Acquire(ctx, "stale", "s1", "old:host", time.Second)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// Backdate the owner.json created field.
	ld, _ := m.lockDir("stale", "s1")
	of := ownerFilePath(ld)
	meta, _ := readOwnerFile(of)
	meta.Created = time.Now().Add(-10 * time.Second).Unix()
	data, _ := json.Marshal(meta)
	os.WriteFile(of, data, 0600)

	// Second acquire should break the stale lock and succeed.
	err = m.Acquire(ctx, "stale", "s1", "new:host", time.Second)
	if err != nil {
		t.Fatalf("acquire stale: %v", err)
	}
	m.Release(ctx, "stale", "s1", "new:host")
}

func TestReleaseOwnerVerification(t *testing.T) {
	m := setupTestManager(t)
	ctx := context.Background()

	err := m.Acquire(ctx, "owned", "s1", "alice:host", time.Second)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// Wrong owner cannot release.
	err = m.Release(ctx, "owned", "s1", "bob:host")
	if !errors.Is(err, ErrNotOwner) {
		t.Errorf("release by wrong owner = %v, want ErrNotOwner", err)
	}

	// Correct owner can release.
	err = m.Release(ctx, "owned", "s1", "alice:host")
	if err != nil {
		t.Fatalf("release by owner: %v", err)
	}
}

func TestReleaseNotFound(t *testing.T) {
	m := setupTestManager(t)
	ctx := context.Background()

	err := m.Release(ctx, "nonexistent", "s1", "anyone:host")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("release nonexistent = %v, want ErrNotFound", err)
	}
}

func TestList(t *testing.T) {
	m := setupTestManager(t)
	ctx := context.Background()

	m.Acquire(ctx, "lock1", "s1", "a:host", time.Second)
	m.Acquire(ctx, "lock2", "s2", "b:host", time.Second)

	locks, err := m.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(locks) != 2 {
		t.Errorf("list count = %d, want 2", len(locks))
	}

	m.Release(ctx, "lock1", "s1", "a:host")
	m.Release(ctx, "lock2", "s2", "b:host")
}

func TestClean(t *testing.T) {
	m := setupTestManager(t)
	ctx := context.Background()

	// Acquire and backdate.
	m.Acquire(ctx, "old", "s1", "99999:host", time.Second)
	ld, _ := m.lockDir("old", "s1")
	of := ownerFilePath(ld)
	meta, _ := readOwnerFile(of)
	meta.Created = time.Now().Add(-10 * time.Second).Unix()
	meta.PID = 99999 // Very likely not a real running process.
	data, _ := json.Marshal(meta)
	os.WriteFile(of, data, 0600)

	removed, err := m.Clean(ctx, time.Second)
	if err != nil {
		t.Fatalf("clean: %v", err)
	}
	if removed != 1 {
		t.Errorf("clean removed = %d, want 1", removed)
	}

	// Verify it's gone.
	locks, _ := m.List(ctx)
	if len(locks) != 0 {
		t.Errorf("list after clean = %d, want 0", len(locks))
	}
}

func TestConcurrentAcquire(t *testing.T) {
	m := setupTestManager(t)
	m.StaleAge = 30 * time.Second // Long stale age — active locks should never be broken.
	ctx := context.Background()

	const goroutines = 10
	var holding atomic.Int32
	var maxHolding atomic.Int32
	var wg sync.WaitGroup

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			// Each goroutine uses a unique owner to prevent cross-release.
			owner := fmt.Sprintf("%d:host-%d", os.Getpid(), id)
			err := m.Acquire(ctx, "race", "s1", owner, 5*time.Second)
			if err != nil {
				t.Errorf("goroutine %d acquire: %v", id, err)
				return
			}

			cur := holding.Add(1)
			if cur > 1 {
				maxHolding.Store(cur)
			}

			time.Sleep(time.Millisecond) // Hold briefly.

			holding.Add(-1)
			m.Release(ctx, "race", "s1", owner)
		}(i)
	}

	wg.Wait()
	if max := maxHolding.Load(); max > 1 {
		t.Errorf("max concurrent holders = %d, want 1", max)
	}
}

func TestPathTraversal(t *testing.T) {
	m := setupTestManager(t)
	ctx := context.Background()

	cases := []struct {
		name, scope string
	}{
		{"../escape", "s1"},
		{"test", "../escape"},
		{"../../etc", "passwd"},
		{"name/with/slash", "s1"},
		{"..", "s1"},
		{".", "s1"},
		{"", "s1"},
		{"test", ""},
	}

	for _, tc := range cases {
		err := m.Acquire(ctx, tc.name, tc.scope, "a:host", 100*time.Millisecond)
		if !errors.Is(err, ErrBadName) {
			t.Errorf("Acquire(%q, %q) = %v, want ErrBadName", tc.name, tc.scope, err)
		}
	}
}

func TestGhostLockCleanup(t *testing.T) {
	m := setupTestManager(t)
	ctx := context.Background()

	// Create a ghost lock: dir exists but no owner.json.
	ld, _ := m.lockDir("ghost", "s1")
	os.MkdirAll(ld, 0700)

	// Backdate the dir mtime.
	past := time.Now().Add(-10 * time.Second)
	os.Chtimes(ld, past, past)

	// Stale should find it via dir mtime fallback.
	stale, err := m.Stale(ctx, time.Second)
	if err != nil {
		t.Fatalf("stale: %v", err)
	}
	if len(stale) != 1 {
		t.Fatalf("stale count = %d, want 1", len(stale))
	}

	// Clean should remove it (PID=0, not alive).
	removed, err := m.Clean(ctx, time.Second)
	if err != nil {
		t.Fatalf("clean: %v", err)
	}
	if removed != 1 {
		t.Errorf("clean removed = %d, want 1", removed)
	}
}
