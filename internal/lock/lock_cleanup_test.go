package lock

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Regression tests for ownership-safe removal, deadline handling, and the
// cleaner protocol shared with the bash fallback in hooks/lib-intercore.sh.
// They swap package-level test seams, so none of them run in parallel.

const (
	deadPidA = 2147000001 // never a real process: liveness is faked for these
	deadPidB = 2147000002
)

// fakeLiveness makes the given pids look dead and leaves all others to pidAlive.
func fakeLiveness(t *testing.T, dead ...int) {
	t.Helper()
	old := livenessCheck
	livenessCheck = func(pid int) bool {
		for _, d := range dead {
			if pid == d {
				return false
			}
		}
		return pidAlive(pid)
	}
	t.Cleanup(func() { livenessCheck = old })
}

func setHook(t *testing.T, p *func(string), f func(string)) {
	t.Helper()
	old := *p
	*p = f
	t.Cleanup(func() { *p = old })
}

func hostName() string { h, _ := os.Hostname(); return h }

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func writeFileT(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
}

// seedOwner creates the lock directory with an owner record and returns the
// directory and the exact record bytes.
func seedOwner(t *testing.T, m *Manager, name, scope string, meta ownerMeta) (string, []byte) {
	t.Helper()
	ld, err := m.lockDir(name, scope)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(ld, 0700); err != nil {
		t.Fatal(err)
	}
	b := mustJSON(t, meta)
	writeFileT(t, filepath.Join(ld, "owner.json"), b)
	return ld, b
}

func deadOwner(pid int) ownerMeta {
	return ownerMeta{PID: pid, Host: hostName(), Owner: strconv.Itoa(pid) + ":" + hostName(), Created: 1}
}

func backdate(t *testing.T, path string) {
	t.Helper()
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatal(err)
	}
}

func readBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Base(path), err)
	}
	return b
}

func exists(path string) bool { _, err := os.Lstat(path); return err == nil }

// swapDirForForeign moves the lock directory aside (never deleting it, so its
// inode cannot be reused) and puts a new directory holding foreign in its place.
func swapDirForForeign(t *testing.T, ld string, foreign []byte) {
	t.Helper()
	if err := os.Rename(ld, ld+".moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(ld, 0700); err != nil {
		t.Fatal(err)
	}
	if foreign != nil {
		writeFileT(t, filepath.Join(ld, "owner.json"), foreign)
	}
}

// --- 1. Ownership-safe removal ---------------------------------------------

// failWithSwap installs write (or close) failure seams that first put a
// foreign directory in place of the lock directory the failing attempt created,
// once, then fail. It returns the number of times a lock directory was created.
func failWithSwap(t *testing.T, ld string, foreign []byte, closeFails bool) *int32 {
	t.Helper()
	oldW, oldC := pendingWrite, pendingClose
	t.Cleanup(func() { pendingWrite, pendingClose = oldW, oldC })
	var once sync.Once
	boom := errors.New("injected failure")
	swap := func() { once.Do(func() { swapDirForForeign(t, ld, foreign) }) }
	if closeFails {
		pendingClose = func(f *os.File) error { f.Close(); swap(); return boom }
	} else {
		pendingWrite = func(*os.File, []byte) error { swap(); return boom }
	}
	var mkdirs int32
	setHook(t, &afterMkdirHook, func(string) { atomic.AddInt32(&mkdirs, 1) })
	return &mkdirs
}

func TestWriteFailureNeverRemovesReplacementRecord(t *testing.T) {
	for _, closeFails := range []bool{false, true} {
		name := "write"
		if closeFails {
			name = "close"
		}
		t.Run(name, func(t *testing.T) {
			m := setupTestManager(t)
			ld, _ := m.lockDir("w", "s")
			foreign := mustJSON(t, ownerMeta{PID: os.Getpid(), Host: hostName(), Owner: "foreign:h", Created: time.Now().Unix() + 3600})
			mkdirs := failWithSwap(t, ld, foreign, closeFails)

			err := m.Acquire(context.Background(), "w", "s", "1:h", 250*time.Millisecond)
			if err == nil {
				t.Fatal("acquire succeeded despite a failing record write")
			}
			got, rerr := os.ReadFile(filepath.Join(ld, "owner.json"))
			if rerr != nil || !bytes.Equal(got, foreign) {
				t.Fatalf("replacement owner.json lost or changed (err=%v): %s", rerr, got)
			}
			if n := atomic.LoadInt32(mkdirs); n != 1 {
				t.Fatalf("directory was created %d times: the replacement was removed", n)
			}
		})
	}
}

func TestWriteFailureNeverRemovesReplacementDirectory(t *testing.T) {
	m := setupTestManager(t)
	ld, _ := m.lockDir("w", "s")
	mkdirs := failWithSwap(t, ld, nil, false) // an empty directory that is not ours
	if err := m.Acquire(context.Background(), "w", "s", "1:h", 150*time.Millisecond); err == nil {
		t.Fatal("acquire succeeded despite a failing record write")
	}
	if !exists(ld) {
		t.Fatal("replacement directory was removed")
	}
	if n := atomic.LoadInt32(mkdirs); n != 1 {
		t.Fatalf("directory was created %d times: the replacement was removed", n)
	}
}

// --- 2. Deadline ------------------------------------------------------------

func TestDeadlineHeldAfterEEXISTRetry(t *testing.T) {
	m := setupTestManager(t)
	m.StaleAge = time.Hour
	ld, _ := seedOwner(t, m, "d", "s", ownerMeta{PID: os.Getpid(), Host: hostName(), Owner: "holder:h", Created: time.Now().Unix()})
	// The holder lets go well before the first retry would run, but after the deadline.
	go func() { time.Sleep(60 * time.Millisecond); os.Remove(filepath.Join(ld, "owner.json")); os.Remove(ld) }()
	start := time.Now()
	err := m.Acquire(context.Background(), "d", "s", "1:h", 20*time.Millisecond)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v after %v, want ErrTimeout: acquired past a 20ms deadline", err, time.Since(start))
	}
}

func TestDeadlineHeldAfterWriteFailureRetry(t *testing.T) {
	m := setupTestManager(t)
	var first int32
	setHook(t, &afterMkdirHook, func(d string) {
		if atomic.AddInt32(&first, 1) == 1 {
			os.Remove(d) // the first record write fails: its directory is gone
		}
	})
	start := time.Now()
	err := m.Acquire(context.Background(), "d", "s", "1:h", 20*time.Millisecond)
	if err == nil {
		t.Fatalf("acquired after %v on a 20ms deadline", time.Since(start))
	}
}

func TestDeadlineHeldAfterStaleBreak(t *testing.T) {
	m := setupTestManager(t)
	m.StaleAge = time.Millisecond
	ld, _ := seedOwner(t, m, "d", "s", deadOwner(deadPidA))
	old := livenessCheck
	livenessCheck = func(pid int) bool {
		if pid == deadPidA {
			time.Sleep(100 * time.Millisecond) // a slow check, as on a loaded host
			return false
		}
		return pidAlive(pid)
	}
	t.Cleanup(func() { livenessCheck = old })

	start := time.Now()
	err := m.Acquire(context.Background(), "d", "s", "1:h", 20*time.Millisecond)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v after %v, want ErrTimeout: acquired past a 20ms deadline", err, time.Since(start))
	}
	_ = ld
}

// --- 3. Cleaner protocol ------------------------------------------------------

func TestCleanDefersToLiveCleanerClaim(t *testing.T) {
	m := setupTestManager(t)
	fakeLiveness(t, deadPidA)
	ld, rec := seedOwner(t, m, "c", "s", deadOwner(deadPidA))
	claim := mustJSON(t, claimMeta{PID: os.Getpid(), Host: hostName(), Created: time.Now().Unix(), Token: "live"})
	writeFileT(t, filepath.Join(ld, "cleaner.json"), claim)

	removed, err := m.Clean(context.Background(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Fatalf("removed = %d, want 0 while another cleaner holds the claim", removed)
	}
	if !bytes.Equal(readBytes(t, filepath.Join(ld, "owner.json")), rec) {
		t.Fatal("owner record changed under a live claim")
	}
	if !bytes.Equal(readBytes(t, filepath.Join(ld, "cleaner.json")), claim) {
		t.Fatal("another cleaner's claim was removed")
	}
}

// A cleaner decides record A is dead and stalls; A's lock is cleaned by someone
// else and live B takes the lock; the stalled cleaner resumes. B must survive.
func TestPausedCleanerNeverRemovesSuccessorRecord(t *testing.T) {
	m := setupTestManager(t)
	ld, _ := seedOwner(t, m, "p", "s", deadOwner(deadPidA))
	paused, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	old := livenessCheck
	livenessCheck = func(pid int) bool {
		if pid == deadPidA {
			once.Do(func() { close(paused) })
			<-resume
			return false
		}
		return pidAlive(pid)
	}
	t.Cleanup(func() { livenessCheck = old })

	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() {
		n, err := m.Clean(context.Background(), time.Second)
		done <- result{n, err}
	}()
	<-paused
	// Meanwhile A is cleaned up and live B takes the lock.
	if err := os.Remove(filepath.Join(ld, "owner.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(ld); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(ld, 0700); err != nil {
		t.Fatal(err)
	}
	b := mustJSON(t, ownerMeta{PID: os.Getpid(), Host: hostName(), Owner: "b:h", Created: 1, Token: "b"})
	writeFileT(t, filepath.Join(ld, "owner.json"), b)
	close(resume)
	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.n != 0 {
		t.Fatalf("removed = %d, want 0", r.n)
	}
	if got := readBytes(t, filepath.Join(ld, "owner.json")); !bytes.Equal(got, b) {
		t.Fatalf("successor's record was changed: %s", got)
	}
	if exists(filepath.Join(ld, "cleaner.json")) {
		t.Fatal("cleaner left its claim behind")
	}
}

func TestDeadCleanerClaimIsTakenOver(t *testing.T) {
	m := setupTestManager(t)
	m.StaleAge = 50 * time.Millisecond
	fakeLiveness(t, deadPidA, deadPidB)
	ld, _ := seedOwner(t, m, "t", "s", deadOwner(deadPidA))
	writeFileT(t, filepath.Join(ld, "cleaner.json"),
		mustJSON(t, claimMeta{PID: deadPidB, Host: hostName(), Created: time.Now().Unix(), Token: "dead"}))

	if err := m.Acquire(context.Background(), "t", "s", "1:h", 2*time.Second); err != nil {
		t.Fatalf("a dead cleaner's claim wedged the lock: %v", err)
	}
	ents, _ := os.ReadDir(ld)
	for _, e := range ents {
		if e.Name() != "owner.json" {
			t.Fatalf("leftover %q in lock directory", e.Name())
		}
	}
}

func TestOldCleanerClaimIsTakenOverEvenIfPidLooksAlive(t *testing.T) {
	m := setupTestManager(t)
	m.StaleAge = 50 * time.Millisecond
	fakeLiveness(t, deadPidA)
	ld, _ := seedOwner(t, m, "t", "s", deadOwner(deadPidA))
	// pid reuse: the claim names a live process but is far older than the bound.
	writeFileT(t, filepath.Join(ld, "cleaner.json"),
		mustJSON(t, claimMeta{PID: os.Getpid(), Host: hostName(), Created: 1, Token: "old"}))
	if err := m.Acquire(context.Background(), "t", "s", "1:h", 2*time.Second); err != nil {
		t.Fatalf("an old cleaner claim wedged the lock: %v", err)
	}
}

func TestFreshClaimOfLiveCleanerBlocksBreak(t *testing.T) {
	m := setupTestManager(t)
	m.StaleAge = 50 * time.Millisecond
	fakeLiveness(t, deadPidA)
	ld, rec := seedOwner(t, m, "t", "s", deadOwner(deadPidA))
	claim := mustJSON(t, claimMeta{PID: os.Getpid(), Host: hostName(), Created: time.Now().Unix(), Token: "live"})
	writeFileT(t, filepath.Join(ld, "cleaner.json"), claim)
	if err := m.Acquire(context.Background(), "t", "s", "1:h", 200*time.Millisecond); !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout while a live cleaner holds the claim", err)
	}
	if !bytes.Equal(readBytes(t, filepath.Join(ld, "owner.json")), rec) ||
		!bytes.Equal(readBytes(t, filepath.Join(ld, "cleaner.json")), claim) {
		t.Fatal("a live cleaner's state was disturbed")
	}
}

func TestOnlyOwnClaimIsRemoved(t *testing.T) {
	m := setupTestManager(t)
	fakeLiveness(t, deadPidA)
	ld, _ := seedOwner(t, m, "o", "s", deadOwner(deadPidA))
	other := mustJSON(t, claimMeta{PID: os.Getpid(), Host: hostName(), Created: time.Now().Unix(), Token: "someone-else"})
	// After this cleaner takes its claim, its claim is displaced by another's.
	setHook(t, &afterClaimHook, func(d string) { writeFileT(t, filepath.Join(d, "cleaner.json"), other) })

	if _, err := m.Clean(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
	if !exists(ld) {
		// the foreign claim keeps the directory in place
		t.Fatal("directory removed from under another cleaner's claim")
	}
	if got := readBytes(t, filepath.Join(ld, "cleaner.json")); !bytes.Equal(got, other) {
		t.Fatalf("cleaner removed a claim that was not its own: %s", got)
	}
}

func TestPendingOnlyDirWithDeadWriterIsReclaimable(t *testing.T) {
	fakeLiveness(t, deadPidA)
	pending := ".pending." + strconv.Itoa(deadPidA) + ".1.abc"

	t.Run("acquire", func(t *testing.T) {
		m := setupTestManager(t)
		m.StaleAge = time.Hour // age alone would never free it
		ld, _ := m.lockDir("q", "s")
		if err := os.MkdirAll(ld, 0700); err != nil {
			t.Fatal(err)
		}
		writeFileT(t, filepath.Join(ld, pending), []byte(`{"pid":`))
		if err := m.Acquire(context.Background(), "q", "s", "1:h", time.Second); err != nil {
			t.Fatalf("a dead writer's pending file wedged the lock: %v", err)
		}
		if exists(filepath.Join(ld, pending)) {
			t.Fatal("dead writer's pending file left behind")
		}
	})
	t.Run("clean", func(t *testing.T) {
		m := setupTestManager(t)
		ld, _ := m.lockDir("q", "s")
		if err := os.MkdirAll(ld, 0700); err != nil {
			t.Fatal(err)
		}
		writeFileT(t, filepath.Join(ld, pending), []byte(`{"pid":`))
		backdate(t, ld)
		n, err := m.Clean(context.Background(), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 || exists(ld) {
			t.Fatalf("removed = %d, dir present = %v; want the pending-only directory reclaimed", n, exists(ld))
		}
	})
}

func TestLivePendingIsNotRemoved(t *testing.T) {
	m := setupTestManager(t)
	ld, _ := m.lockDir("q", "s")
	if err := os.MkdirAll(ld, 0700); err != nil {
		t.Fatal(err)
	}
	pending := filepath.Join(ld, ".pending."+strconv.Itoa(os.Getpid())+".1.abc") // a writer mid-publish
	writeFileT(t, pending, []byte(`{"pid":`))
	backdate(t, ld)
	if n, _ := m.Clean(context.Background(), time.Second); n != 0 {
		t.Fatalf("removed = %d, want 0", n)
	}
	if err := m.Acquire(context.Background(), "q", "s", "1:h", 150*time.Millisecond); !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if !exists(pending) {
		t.Fatal("a live writer's pending file was removed")
	}
}

func TestClaimOnlyDirIsReclaimedOnlyWhenAbandoned(t *testing.T) {
	fakeLiveness(t, deadPidA)
	m := setupTestManager(t)
	m.StaleAge = time.Hour
	ld, _ := m.lockDir("q", "s")
	if err := os.MkdirAll(ld, 0700); err != nil {
		t.Fatal(err)
	}
	live := mustJSON(t, claimMeta{PID: os.Getpid(), Host: hostName(), Created: time.Now().Unix()})
	writeFileT(t, filepath.Join(ld, "cleaner.json"), live)
	if err := m.Acquire(context.Background(), "q", "s", "1:h", 150*time.Millisecond); !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout behind a live claim", err)
	}
	writeFileT(t, filepath.Join(ld, "cleaner.json"),
		mustJSON(t, claimMeta{PID: deadPidA, Host: hostName(), Created: time.Now().Unix()}))
	if err := m.Acquire(context.Background(), "q", "s", "1:h", time.Second); err != nil {
		t.Fatalf("a dead cleaner's claim-only directory was not reclaimed: %v", err)
	}
}

// --- Release checks the acquisition token --------------------------------------

func TestReleaseChecksAcquisitionToken(t *testing.T) {
	m := setupTestManager(t)
	ctx := context.Background()
	tok, err := m.AcquireToken(ctx, "r", "s", "1:h", time.Second)
	if err != nil || tok == "" {
		t.Fatalf("acquire: tok=%q err=%v", tok, err)
	}
	if err := m.ReleaseToken(ctx, "r", "s", "1:h", "not-the-token"); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("wrong token: err = %v, want ErrNotOwner", err)
	}
	ld, _ := m.lockDir("r", "s")
	if !exists(filepath.Join(ld, "owner.json")) {
		t.Fatal("wrong-token release removed the record")
	}
	if err := m.ReleaseToken(ctx, "r", "s", "1:h", tok); err != nil {
		t.Fatalf("release with the token: %v", err)
	}
	if exists(ld) {
		t.Fatal("lock directory left behind")
	}
}

// A delayed Release by a predecessor must not remove a successor's record that
// carries the same owner string.
func TestDelayedReleaseKeepsSuccessorWithSameOwner(t *testing.T) {
	m := setupTestManager(t)
	ctx := context.Background()
	if err := m.Acquire(ctx, "r", "s", "1:h", time.Second); err != nil {
		t.Fatal(err)
	}
	ld, _ := m.lockDir("r", "s")
	// The predecessor's lock is broken and the same owner string acquires anew,
	// through another Manager (another process in real use).
	if err := os.Remove(filepath.Join(ld, "owner.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(ld); err != nil {
		t.Fatal(err)
	}
	m2 := NewManager(m.BaseDir)
	if err := m2.Acquire(ctx, "r", "s", "1:h", time.Second); err != nil {
		t.Fatal(err)
	}
	succ := readBytes(t, filepath.Join(ld, "owner.json"))

	if err := m.Release(ctx, "r", "s", "1:h"); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("delayed release: err = %v, want ErrNotOwner", err)
	}
	if !bytes.Equal(readBytes(t, filepath.Join(ld, "owner.json")), succ) {
		t.Fatal("successor's record was removed")
	}
	if err := m2.Release(ctx, "r", "s", "1:h"); err != nil {
		t.Fatalf("successor's release: %v", err)
	}
}

func TestReleaseOfTokenlessRecordMatchesOnOwner(t *testing.T) {
	m := setupTestManager(t)
	ctx := context.Background()
	ld, _ := seedOwner(t, m, "r", "s", ownerMeta{PID: 1, Host: "h", Owner: "1:h", Created: time.Now().Unix()})
	if err := m.ReleaseToken(ctx, "r", "s", "1:h", "any-token"); err != nil {
		t.Fatalf("record without a token must release on owner match: %v", err)
	}
	if exists(ld) {
		t.Fatal("lock directory left behind")
	}
	ld, _ = seedOwner(t, m, "r", "s", ownerMeta{PID: 1, Host: "h", Owner: "1:h", Created: time.Now().Unix()})
	if err := m.Release(ctx, "r", "s", "1:h"); err != nil {
		t.Fatalf("Release of a record without a token: %v", err)
	}
	if exists(ld) {
		t.Fatal("lock directory left behind")
	}
	if err := m.Release(ctx, "r", "s", "1:h"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// Separate Managers share nothing but the directory: exactly one may hold the
// lock at a time.
func TestExclusiveAcrossManagers(t *testing.T) {
	base := filepath.Join(t.TempDir(), "locks")
	var inside, violations, entered int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m := NewManager(base)
			owner := strconv.Itoa(1000+i) + ":h"
			for j := 0; j < 10; j++ {
				if err := m.Acquire(context.Background(), "x", "s", owner, 10*time.Second); err != nil {
					t.Errorf("acquire: %v", err)
					return
				}
				if atomic.AddInt32(&inside, 1) != 1 {
					atomic.AddInt32(&violations, 1)
				}
				atomic.AddInt32(&entered, 1)
				time.Sleep(time.Millisecond)
				atomic.AddInt32(&inside, -1)
				if err := m.Release(context.Background(), "x", "s", owner); err != nil {
					t.Errorf("release: %v", err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	if violations != 0 || entered != 80 {
		t.Fatalf("violations = %d, entered = %d (want 0, 80)", violations, entered)
	}
}
