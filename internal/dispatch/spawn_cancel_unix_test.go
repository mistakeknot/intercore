//go:build !windows

package dispatch

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mistakeknot/intercore/internal/db"
)

func TestSpawnGeneratedCancellationRecovery(t *testing.T) {
	for _, boundary := range []string{"before-admission", "after-identity", "after-exit"} {
		for _, order := range recoveryOrders("kill", "collect") {
			t.Run(boundary+"/"+strings.Join(order, "-"), func(t *testing.T) {
				ctx := context.Background()
				store, path := cancelTestStore(t)
				other := runningDispatch(t, store)
				before, err := store.Get(ctx, other)
				if err != nil {
					t.Fatal(err)
				}
				opts := admissionOptions(t)
				// A real owned process, not a fake liveness or termination checker.
				if err := os.WriteFile(opts.DispatchSH, []byte("#!/bin/sh\nexec sleep 60\n"), 0700); err != nil {
					t.Fatal(err)
				}
				spawnCtx, cancel := context.WithCancel(ctx)
				defer cancel()
				if boundary == "before-admission" {
					cancel()
				}
				result, err := Spawn(spawnCtx, store, opts)
				if boundary == "before-admission" {
					if !errors.Is(err, context.Canceled) || result != nil {
						t.Fatalf("cancelled spawn = %+v, %v", result, err)
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					var reap sync.Once
					stop := func() { reap.Do(func() { _ = syscall.Kill(-result.PID, syscall.SIGKILL); _ = result.Cmd.Wait() }) }
					t.Cleanup(stop)
					cancel()
					if boundary == "after-exit" {
						stop()
					}
					// Losing the caller does not authorize a cancelled cleanup to
					// discard its slot, or to touch the other attempt's slot.
					if err := Kill(spawnCtx, store, result.ID); !errors.Is(err, context.Canceled) {
						t.Fatalf("cancelled cleanup: %v", err)
					}
					d, err := store.Get(ctx, result.ID)
					if err != nil || d.Status != StatusRunning {
						t.Fatalf("cancelled cleanup changed attempt: %+v, %v", d, err)
					}
					if boundary == "after-identity" {
						if err := Kill(ctx, store, result.ID); err != nil {
							t.Fatal(err)
						}
						stop()
					}
					// Recover through a separate DB handle as a restarted caller.
					reopened, err := db.Open(path, time.Second)
					if err != nil {
						t.Fatal(err)
					}
					defer reopened.Close()
					recovery := New(reopened.SqlDB(), nil)
					for _, action := range append(append([]string{}, order...), order...) {
						if action == "kill" {
							err = Kill(ctx, recovery, result.ID)
						} else {
							err = Collect(ctx, recovery, result.ID)
						}
						if err != nil {
							t.Fatal(err)
						}
						assertOneTerminal(t, recovery, result.ID)
					}
					if err := syscall.Kill(result.PID, 0); !errors.Is(err, syscall.ESRCH) {
						t.Fatalf("worker still exists after reap: %v", err)
					}
				}
				after, err := store.Get(ctx, other)
				if err != nil || !reflect.DeepEqual(before, after) {
					t.Fatal("cancellation released another attempt")
				}
				_, err = store.admit(ctx, &Dispatch{AgentType: "codex", ProjectDir: t.TempDir()}, SpawnOptions{Policy: &SpawnPolicy{MaxActiveGlobal: 1}})
				requireRejection(t, err, "concurrency_limit_global")
			})
		}
	}
}

func cancelTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open(path, time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	if err := d.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return New(d.SqlDB(), nil), path
}

// holdWriteLock takes SQLite's write lock from a separate handle, as a writer in
// another process would, until release runs.
func holdWriteLock(t *testing.T, path string) (release func()) {
	t.Helper()
	holder, err := db.Open(path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := holder.SqlDB().Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
			conn.Close()
			holder.Close()
		})
	}
	t.Cleanup(release)
	return release
}

func awaitCancelled(t *testing.T, done <-chan error, what string) {
	t.Helper()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("%s: err = %v, want context.Canceled", what, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not return after cancellation", what)
	}
}

// Identity recording that is cancelled while it waits inside its transaction
// reports the cancellation and records nothing.
func TestRecordProcessIdentityCancelledInsideTransaction(t *testing.T) {
	ctx := context.Background()
	store, path := cancelTestStore(t)
	id, err := store.Create(ctx, &Dispatch{AgentType: "codex", ProjectDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = platformSysProcAttr()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go cmd.Wait()
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })

	release := holdWriteLock(t, path)
	recordCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := store.recordProcessIdentity(recordCtx, id, cmd.Process.Pid)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("identity recording finished while another writer held the lock: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	cancel()
	release()
	awaitCancelled(t, done, "recordProcessIdentity")

	d, err := store.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != StatusSpawned || d.PID != nil {
		t.Fatalf("dispatch status %s pid %v after cancelled identity recording, want spawned without pid", d.Status, d.PID)
	}
	var identities int
	if err := store.db.QueryRowContext(ctx, "SELECT count(*) FROM state WHERE key = 'dispatch.process' AND scope_id = ?", id).Scan(&identities); err != nil {
		t.Fatal(err)
	}
	if identities != 0 {
		t.Fatalf("identity rows = %d, want 0", identities)
	}
}

// Spawn cancelled while admission waits for the write lock reports the
// cancellation, admits nothing and starts nothing.
func TestSpawnCancelledDuringAdmissionStartsNothing(t *testing.T) {
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	wrapper := filepath.Join(dir, "dispatch.sh")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\ntouch \"$IC_TEST_STARTED\"\nsleep 60\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("IC_TEST_STARTED", started)
	prompt := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(prompt, []byte("test prompt"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, path := cancelTestStore(t)

	release := holdWriteLock(t, path)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		result, err := Spawn(ctx, store, SpawnOptions{AgentType: "codex", ProjectDir: dir, PromptFile: prompt, DispatchSH: wrapper})
		if result != nil {
			_ = syscall.Kill(-result.PID, syscall.SIGKILL)
			_ = result.Cmd.Wait()
		}
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	awaitCancelled(t, done, "Spawn")
	release()

	if _, err := os.Stat(started); !os.IsNotExist(err) {
		t.Fatalf("worker started despite cancelled admission (stat err %v)", err)
	}
	all, err := store.List(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 0 {
		t.Fatalf("dispatches = %d after cancelled admission, want 0", len(all))
	}
}
