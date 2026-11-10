package dispatch

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// recordDeadIdentity stores the birth identity Spawn would have recorded for the
// dispatch's (long gone) process, which is what proves it dead rather than guessed.
func recordDeadIdentity(t *testing.T, store *Store, id string) {
	t.Helper()
	d, err := store.Get(context.Background(), id)
	if err != nil || d.PID == nil {
		t.Fatalf("Get %s: %v (pid %v)", id, err, d)
	}
	payload, err := json.Marshal(processIdentity{Version: processIdentityVersion, PID: *d.PID, GroupID: *d.PID, Birth: "gone"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO state(key,scope_id,payload,updated_at) VALUES ('dispatch.process',?,?,unixepoch())`, id, string(payload)); err != nil {
		t.Fatal(err)
	}
}

func statusOf(t *testing.T, store *Store, id string) string {
	t.Helper()
	d, err := store.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get %s: %v", id, err)
	}
	return d.Status
}

func TestReapCollectsDeadProcessAndLeavesSupervisedAlone(t *testing.T) {
	ctx := context.Background()
	store, _ := cancelTestStore(t)
	dead, supervised := runningDispatch(t, store), runningDispatch(t, store)
	recordDeadIdentity(t, store, dead)
	superviseDispatch(t, store, supervised)

	result, err := store.Reap(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Collected) != 1 || result.Collected[0] != dead || len(result.Released) != 0 {
		t.Fatalf("Reap = %+v, want only %s collected", result, dead)
	}
	if got := statusOf(t, store, dead); got != StatusFailed {
		t.Errorf("dead dispatch = %q, want failed", got)
	}
	// Only the supervisor may write a supervised dispatch's outcome.
	if got := statusOf(t, store, supervised); got != StatusRunning {
		t.Errorf("supervised dispatch = %q, want it untouched", got)
	}
}

func TestReapReleasesOnlyStaleReservations(t *testing.T) {
	ctx := context.Background()
	store, _ := cancelTestStore(t)
	create := func() string {
		id, err := store.Create(ctx, &Dispatch{AgentType: "codex", ProjectDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	fresh, stale, staleSupervised := create(), create(), create()
	superviseDispatch(t, store, staleSupervised)
	old := time.Now().Add(-time.Hour).Unix()
	for _, id := range []string{stale, staleSupervised} {
		if _, err := store.db.Exec("UPDATE dispatches SET created_at = ? WHERE id = ?", old, id); err != nil {
			t.Fatal(err)
		}
	}

	result, err := store.Reap(ctx, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Released) != 1 || result.Released[0] != stale || len(result.Collected) != 0 {
		t.Fatalf("Reap = %+v, want only %s released", result, stale)
	}
	if got := statusOf(t, store, stale); got != StatusCancelled {
		t.Errorf("stale reservation = %q, want cancelled", got)
	}
	if got := statusOf(t, store, fresh); got != StatusSpawned {
		t.Errorf("reservation inside its lease = %q, want spawned", got)
	}
	if got := statusOf(t, store, staleSupervised); got != StatusSpawned {
		t.Errorf("supervised reservation = %q, want it untouched", got)
	}
	if n, err := store.CountActiveGlobal(ctx); err != nil || n != 2 {
		t.Errorf("active slots = %d, %v; want 2 (the fresh and supervised rows)", n, err)
	}
}

func TestStopByRunSkipsSupervisedAndProcesslessRows(t *testing.T) {
	ctx := context.Background()
	store, _ := cancelTestStore(t)
	run := "run"
	supervised := runningDispatch(t, store)
	superviseDispatch(t, store, supervised)
	processless, err := store.Create(ctx, &Dispatch{AgentType: "codex", ProjectDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{supervised, processless} {
		if _, err := store.db.Exec("UPDATE dispatches SET scope_id = ? WHERE id = ?", run, id); err != nil {
			t.Fatal(err)
		}
	}

	stopped, err := StopByRun(ctx, store, run)
	if err != nil || stopped != 0 {
		t.Fatalf("StopByRun = %d, %v; want 0, nil", stopped, err)
	}
	if got := statusOf(t, store, supervised); got != StatusRunning {
		t.Errorf("supervised dispatch = %q, want it untouched", got)
	}
	// CancelByRun still owns the row with no process.
	if n, err := store.CancelByRun(ctx, run); err != nil || n != 1 {
		t.Fatalf("CancelByRun = %d, %v; want 1, nil", n, err)
	}
}

// A dead PID alone is not proof: without a recorded birth identity the PID may
// belong to an unrelated process after reuse, so the row must be left running.
func TestReapLeavesDeadPIDWithoutRecordedIdentityAlone(t *testing.T) {
	ctx := context.Background()
	store, _ := cancelTestStore(t)
	legacy := runningDispatch(t, store) // no dispatch.process identity recorded

	result, err := store.Reap(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if result.Total() != 0 {
		t.Fatalf("Reap = %+v, want nothing reaped without a birth identity", result)
	}
	if got := statusOf(t, store, legacy); got != StatusRunning {
		t.Errorf("identity-less dispatch = %q, want running", got)
	}
	if _, err := store.ReapDispatch(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if got := statusOf(t, store, legacy); got != StatusRunning {
		t.Errorf("identity-less dispatch after ReapDispatch = %q, want running", got)
	}
}

// Reap reads a row, then writes. A spawn that records its process in between
// must not have its dispatch cancelled from under it.
func TestReapReleaseNeverCancelsReservationThatStartedAProcess(t *testing.T) {
	ctx := context.Background()
	store, _ := cancelTestStore(t)
	id, err := store.Create(ctx, &Dispatch{AgentType: "codex", ProjectDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec("UPDATE dispatches SET created_at = ? WHERE id = ?", time.Now().Add(-time.Hour).Unix(), id); err != nil {
		t.Fatal(err)
	}
	stale, err := store.Get(ctx, id) // the snapshot Reap decided on: spawned, no pid, old
	if err != nil {
		t.Fatal(err)
	}
	// The spawn wins the race: it records a process and the row becomes running.
	if err := store.UpdateStatus(ctx, id, StatusRunning, UpdateFields{"pid": 999999999}); err != nil {
		t.Fatal(err)
	}

	result := &ReapResult{}
	if err := store.reapOne(ctx, stale, 10*time.Minute, true, result); err != nil {
		t.Fatal(err)
	}
	if result.Total() != 0 {
		t.Fatalf("reapOne = %+v, want nothing released", result)
	}
	if got := statusOf(t, store, id); got != StatusRunning {
		t.Errorf("dispatch that started a process = %q, want running", got)
	}
	if n := terminalRowCount(t, store, "SELECT count(*) FROM dispatch_terminals WHERE dispatch_id = ?", id); n != 0 {
		t.Errorf("terminal records = %d, want 0", n)
	}
}
