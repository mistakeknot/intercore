//go:build icsched_test && linux

package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/mistakeknot/intercore/internal/dispatch"
	"github.com/mistakeknot/intercore/internal/scheduler"
)

// flCapture records the in-flight map the engine hands to onInFlight.
type flCapture struct{ p atomic.Pointer[InFlight] }

func (c *flCapture) hook(f *InFlight) { c.p.Store(f) }

// dropOnce returns a hook that removes the entry the first time it runs for
// a token that satisfies match, and records that it did.
func (c *flCapture) dropOnce(match func(token string) bool, dropped *atomic.Bool) func(string) {
	var once sync.Once
	return func(token string) {
		if !match(token) {
			return
		}
		once.Do(func() {
			if c.p.Load().Drop(token) {
				dropped.Store(true)
			}
		})
	}
}

func (r *rig) dispatchOf(id string) string {
	r.t.Helper()
	var d string
	if err := r.raw.QueryRow(`SELECT COALESCE(dispatch_id,'') FROM scheduler_job_attempts WHERE job_id = ? ORDER BY attempt_no DESC LIMIT 1`, id).Scan(&d); err != nil {
		r.t.Fatal(err)
	}
	return d
}

func (r *rig) launchResult(id string) string {
	r.t.Helper()
	var d string
	if err := r.raw.QueryRow(`SELECT COALESCE(launch_result,'') FROM scheduler_job_attempts WHERE job_id = ? ORDER BY attempt_no DESC LIMIT 1`, id).Scan(&d); err != nil {
		r.t.Fatal(err)
	}
	return d
}

func (r *rig) runnerState() (string, int64) {
	r.t.Helper()
	row, err := r.store.ReadRunner(context.Background())
	if err != nil {
		r.t.Fatal(err)
	}
	return row.State, row.Epoch
}

// E1: SQLITE_BUSY on the next three marker, evidence, Collect and finalize
// statements. The completing stage retries and the job finalizes once, on the
// same runner epoch.
func TestBusyInjectionRetriesWithoutRestart(t *testing.T) {
	busy := errors.New("database is locked (SQLITE_BUSY)")
	runCase := func(t *testing.T, inject bool) (calls map[string]int, r *rig, epoch int64, runErr error) {
		r = newRig(t)
		r.job("j", "ok")
		var mu sync.Mutex
		calls = map[string]int{}
		o := r.opts()
		o.hooks.inject = func(kind string) error {
			mu.Lock()
			defer mu.Unlock()
			calls[kind]++
			if inject && calls[kind] <= 3 {
				return busy
			}
			return nil
		}
		run := start(t, o)
		r.waitStatus("j", scheduler.StatusCompleted)
		run.stop(t)
		mu.Lock()
		defer mu.Unlock()
		out := map[string]int{}
		for k, v := range calls {
			out[k] = v
		}
		return out, r, run.epoch, run.err
	}

	// Negative control: with nothing injected each statement runs once
	// (four marker writes: paths, launch result, running, exited).
	ctl, _, _, _ := runCase(t, false)
	if ctl["marker"] != 4 || ctl["evidence"] != 1 || ctl["collect"] != 1 || ctl["finalize"] != 1 {
		t.Fatalf("control call counts = %v", ctl)
	}

	got, r, epoch, _ := runCase(t, true)
	// Each failed completing step re-enters complete(), so later phases are
	// reached again: Collect runs once per finalize retry as well.
	want := map[string]int{"marker": 7, "evidence": 4, "collect": 7, "finalize": 4}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("injected call counts = %v, want %v", got, want)
		}
	}
	if st, e := r.runnerState(); e != epoch || st != "stopped" {
		t.Fatalf("runner state %q epoch %d, want stopped at %d (no restart)", st, e, epoch)
	}
	if r.attempts("j") != 1 || r.dispatches("j") != 1 || r.runs() != 1 {
		t.Fatalf("attempts=%d dispatches=%d runs=%d", r.attempts("j"), r.dispatches("j"), r.runs())
	}
	if r.outcome("j") != scheduler.ClassOK {
		t.Fatalf("outcome %q", r.outcome("j"))
	}
}

// E2: the map entry is lost at each point where a job can leave it, and the
// current-epoch repair pass finalizes from the persisted marker. An active
// launch seen by the same repair passes is untouched.
func TestRepairFinalizesDroppedEntries(t *testing.T) {
	t.Run("after exit before evidence", func(t *testing.T) {
		r := newRig(t)
		gate := r.gateJob("g")
		r.job("x", "ok")
		var cap flCapture
		var dropped atomic.Bool
		xTok := func(token string) bool {
			a, err := r.store.AttemptByToken(context.Background(), token)
			return err == nil && a.JobID == "x"
		}
		o := r.opts()
		o.hooks.onInFlight = cap.hook
		o.hooks.afterExitBeforeEvidence = cap.dropOnce(xTok, &dropped)
		run := start(t, o)
		r.waitStatus("g", scheduler.StatusRunning)
		r.waitStatus("x", scheduler.StatusCompleted)
		if !dropped.Load() {
			t.Fatal("the hook never dropped the entry: the test proves nothing")
		}
		if r.outcome("x") != scheduler.ClassOK {
			t.Fatalf("x outcome %q", r.outcome("x"))
		}
		// The repair pass that finalized x also saw g and left it alone.
		if st := r.get("g").Status; st != scheduler.StatusRunning {
			t.Fatalf("active job is %s after repair", st)
		}
		if r.attempts("g") != 1 || r.dispatches("g") != 1 {
			t.Fatalf("g attempts=%d dispatches=%d", r.attempts("g"), r.dispatches("g"))
		}
		release(t, gate)
		r.waitStatus("g", scheduler.StatusCompleted)
		run.stop(t)
		if r.runs() != 2 {
			t.Fatalf("runs = %d, want 2 (no relaunch)", r.runs())
		}
	})

	t.Run("after evidence before collect", func(t *testing.T) {
		r := newRig(t)
		r.job("x", "ok")
		var cap flCapture
		var dropped atomic.Bool
		o := r.opts()
		o.hooks.onInFlight = cap.hook
		o.hooks.afterEvidenceBeforeCollect = cap.dropOnce(func(string) bool { return true }, &dropped)
		run := start(t, o)
		r.waitStatus("x", scheduler.StatusCompleted)
		run.stop(t)
		if !dropped.Load() {
			t.Fatal("the hook never dropped the entry")
		}
		if r.outcome("x") != scheduler.ClassOK || r.attempts("x") != 1 || r.runs() != 1 {
			t.Fatalf("outcome=%q attempts=%d runs=%d", r.outcome("x"), r.attempts("x"), r.runs())
		}
	})

	t.Run("after a rejection is persisted", func(t *testing.T) {
		r := newRig(t)
		// One live dispatch fills the global admission limit of 1.
		if _, err := dispatch.New(r.raw, nil).Create(context.Background(), &dispatch.Dispatch{AgentType: "codex", ProjectDir: r.dir}); err != nil {
			t.Fatal(err)
		}
		prompt := filepath.Join(r.dir, "x.prompt")
		so, _ := json.Marshal(map[string]any{"AgentType": "codex", "PromptFile": prompt, "Policy": map[string]any{"MaxActiveGlobal": 1}})
		r.job("x", "ok", func(j *scheduler.SpawnJob) { j.SpawnOpts = string(so) })
		var cap flCapture
		var dropped atomic.Bool
		drop := cap.dropOnce(func(string) bool { return true }, &dropped)
		o := r.opts()
		o.hooks.onInFlight = cap.hook
		o.hooks.afterLaunchPersist = func(token, result string) {
			if result == scheduler.LaunchRejected {
				drop(token)
			}
		}
		run := start(t, o)
		r.waitStatus("x", scheduler.StatusFailed)
		run.stop(t)
		if !dropped.Load() {
			t.Fatal("no rejection was persisted: the test proves nothing")
		}
		if r.launchResult("x") != scheduler.LaunchRejected || r.outcome("x") != scheduler.ClassPrestart {
			t.Fatalf("launch_result=%q outcome=%q", r.launchResult("x"), r.outcome("x"))
		}
		if r.dispatches("x") != 0 || r.runs() != 0 {
			t.Fatalf("dispatches=%d runs=%d", r.dispatches("x"), r.runs())
		}
	})

	t.Run("after a pre-admission spawn error is persisted", func(t *testing.T) {
		r := newRig(t)
		prompt := r.job("x", "ok")
		var cap flCapture
		var dropped atomic.Bool
		drop := cap.dropOnce(func(string) bool { return true }, &dropped)
		o := r.opts()
		o.hooks.onInFlight = cap.hook
		o.hooks.beforeSpawn = func(string) { _ = os.Remove(prompt) } // Spawn fails before admission
		o.hooks.afterLaunchPersist = func(token, result string) {
			if result == scheduler.LaunchSpawnError {
				drop(token)
			}
		}
		run := start(t, o)
		r.waitStatus("x", scheduler.StatusFailed)
		run.stop(t)
		if !dropped.Load() {
			t.Fatal("no spawn error was persisted: the test proves nothing")
		}
		if r.launchResult("x") != scheduler.LaunchSpawnError || r.outcome("x") != scheduler.ClassPrestart {
			t.Fatalf("launch_result=%q outcome=%q", r.launchResult("x"), r.outcome("x"))
		}
		if r.dispatches("x") != 0 || r.runs() != 0 {
			t.Fatalf("dispatches=%d runs=%d", r.dispatches("x"), r.runs())
		}
	})
}

// X1: the strict executor is removed after the claim-time check. Sentinel
// scripts on PATH, in the environment override and at the monorepo search
// path must never run; the job ends as a pre-start failure.
func TestStrictExecutorIsNeverSubstituted(t *testing.T) {
	r := newRig(t)
	tmp := t.TempDir()
	exe := filepath.Join(tmp, "strict-dispatch.sh")
	src, err := os.ReadFile(fakeDispatchSH(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, src, 0o755); err != nil {
		t.Fatal(err)
	}
	sentinelLog := filepath.Join(tmp, "sentinel.log")
	sentinel := "#!/bin/bash\necho ran >> " + sentinelLog + "\nexit 0\n"
	place := func(p string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(sentinel), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	pathDir := filepath.Join(tmp, "bin")
	cwd := filepath.Join(tmp, "cwd")
	place(filepath.Join(pathDir, "dispatch.sh"))
	place(filepath.Join(tmp, "env-dispatch.sh"))
	place(filepath.Join(cwd, "os", "Clavain", "scripts", "dispatch.sh"))
	t.Setenv("PATH", pathDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CLAVAIN_DISPATCH_SH", filepath.Join(tmp, "env-dispatch.sh"))
	o := r.opts() // resolves the fake executor's path before the chdir below
	t.Chdir(cwd)

	r.job("x", "ok")
	var removed atomic.Bool
	o.DispatchSH = exe
	o.hooks.beforeSpawn = func(string) {
		if os.Remove(exe) == nil {
			removed.Store(true)
		}
	}
	run := start(t, o)
	r.waitStatus("x", scheduler.StatusFailed)
	run.stop(t)

	if !removed.Load() {
		t.Fatal("the executor was never removed: the test proves nothing")
	}
	if _, err := os.Stat(sentinelLog); err == nil {
		t.Fatal("a substitute dispatch.sh ran")
	}
	if r.runs() != 0 {
		t.Fatalf("the strict executor ran %d time(s) after removal", r.runs())
	}
	if r.launchResult("x") != scheduler.LaunchSpawnError {
		t.Fatalf("launch_result = %q, want spawn_error", r.launchResult("x"))
	}
	if r.outcome("x") != scheduler.ClassPrestart {
		t.Fatalf("outcome = %q, want prestart", r.outcome("x"))
	}
}

// D1: Collect, a forced kill and the reaper race on one scheduler-linked
// dispatch. One terminal row results, and the job's class agrees with it.
func TestConcurrentCollectKillReapLeaveOneTerminal(t *testing.T) {
	r := newRig(t)
	r.job("h", "hang")
	run := start(t, r.opts())
	r.waitStatus("h", scheduler.StatusRunning)
	var id string
	until(t, "the dispatch to be linked", func() bool { id = r.dispatchOf("h"); return id != "" })
	ds := dispatch.New(r.raw, nil)
	d, err := ds.Get(context.Background(), id)
	if err != nil || d.IsTerminal() {
		t.Fatalf("dispatch before the race: %+v, %v", d, err)
	}

	gun := make(chan struct{})
	var wg sync.WaitGroup
	var returned atomic.Int32
	op := func(f func(ctx context.Context)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gun
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			f(ctx)
			returned.Add(1)
		}()
	}
	op(func(ctx context.Context) { _ = dispatch.Collect(ctx, ds, id) })
	op(func(ctx context.Context) { _ = dispatch.Kill(ctx, ds, id) })
	op(func(ctx context.Context) { _, _ = ds.Reap(ctx, 10*time.Minute) })
	close(gun)
	wg.Wait()
	if returned.Load() != 3 {
		t.Fatalf("only %d of 3 operations returned", returned.Load())
	}

	until(t, "the job to reach a terminal state", func() bool {
		switch r.get("h").Status {
		case scheduler.StatusCancelled, scheduler.StatusCompleted, scheduler.StatusFailed:
			return true
		}
		return false
	})
	run.stop(t)
	if n := r.count(`SELECT COUNT(*) FROM dispatch_terminals WHERE dispatch_id = ?`, id); n != 1 {
		t.Fatalf("terminal rows = %d, want exactly 1", n)
	}
	d, err = ds.Get(context.Background(), id)
	if err != nil || !d.IsTerminal() {
		t.Fatalf("dispatch after the race: %+v, %v", d, err)
	}
	wantClass := map[string]string{
		dispatch.StatusCancelled: scheduler.ClassCancelled,
		dispatch.StatusCompleted: scheduler.ClassOK,
		dispatch.StatusTimeout:   scheduler.ClassTimeout,
	}[d.Status]
	if wantClass != "" && r.outcome("h") != wantClass {
		t.Fatalf("dispatch is %s but the job's class is %q, want %q", d.Status, r.outcome("h"), wantClass)
	}
	if d.Status == dispatch.StatusCancelled && r.get("h").Status != scheduler.StatusCancelled && r.get("h").Status != scheduler.StatusFailed {
		t.Fatalf("job status %s for a cancelled dispatch", r.get("h").Status)
	}
}

// A collect on a live dispatch writes a terminal row but never signals the
// process. The runner started that child, so it must stop it and finalize the
// job; otherwise the waiter blocks for ever and the job holds its slot.
func TestCollectedLiveDispatchEndsItsChild(t *testing.T) {
	r := newRig(t)
	r.job("h", "hang")
	run := start(t, r.opts())
	r.waitStatus("h", scheduler.StatusRunning)
	var id string
	until(t, "the dispatch to be linked", func() bool { id = r.dispatchOf("h"); return id != "" })
	var pid int
	until(t, "the child pid to be recorded", func() bool {
		return r.raw.QueryRow(`SELECT COALESCE(pid,0) FROM dispatches WHERE id = ?`, id).Scan(&pid) == nil && pid > 1
	})
	ds := dispatch.New(r.raw, nil)
	if err := dispatch.Collect(context.Background(), ds, id); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("the child was already gone after Collect, so the test proves nothing: %v", err)
	}
	r.waitStatus("h", scheduler.StatusFailed)
	run.stop(t)
	if err := syscall.Kill(pid, 0); err == nil {
		t.Fatal("the child of a terminal dispatch is still alive")
	}
	if n := r.count(`SELECT COUNT(*) FROM dispatch_terminals WHERE dispatch_id = ?`, id); n != 1 {
		t.Fatalf("terminal rows = %d, want 1", n)
	}
}

// F1: recovery and repair touch only scheduler-linked work. An old orphaned
// dispatch from another feature in the same database is left alone.
func TestOrphanNonSchedulerDispatchIsUntouched(t *testing.T) {
	r := newRig(t)
	ds := dispatch.New(r.raw, nil)
	orphan, err := ds.Create(context.Background(), &dispatch.Dispatch{AgentType: "codex", ProjectDir: r.dir})
	if err != nil {
		t.Fatal(err)
	}
	r.mustExec(`UPDATE dispatches SET created_at = created_at - 3600 WHERE id = ?`, orphan)

	r.job("j", "ok")
	o := r.opts()
	run := start(t, o)
	r.waitStatus("j", scheduler.StatusCompleted)
	// Let several repair passes run after the job: wait for a second job
	// that is itself finalized by a later pass is unnecessary; the repair
	// cadence is 50ms and the control below proves the runner kept working.
	r.job("j2", "ok")
	r.waitStatus("j2", scheduler.StatusCompleted)
	run.stop(t)

	d, err := ds.Get(context.Background(), orphan)
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != dispatch.StatusSpawned || d.IsTerminal() {
		t.Fatalf("orphan dispatch is %s", d.Status)
	}
	if n := r.count(`SELECT COUNT(*) FROM dispatch_terminals WHERE dispatch_id = ?`, orphan); n != 0 {
		t.Fatalf("orphan has %d terminal rows", n)
	}
	// Negative control: the scheduler's own dispatch is terminal.
	jd, err := ds.Get(context.Background(), r.dispatchOf("j"))
	if err != nil || !jd.IsTerminal() {
		t.Fatalf("the job's own dispatch: %+v, %v", jd, err)
	}
}

// A pending job is cancelled directly: no attempt, no dispatch, no run.
func TestCancelPendingJob(t *testing.T) {
	r := newRig(t)
	r.job("c", "ok")
	if _, err := r.store.CancelJob(context.Background(), "c", time.Now()); err != nil {
		t.Fatal(err)
	}
	r.job("ctrl", "ok")
	run := start(t, r.opts())
	r.waitStatus("ctrl", scheduler.StatusCompleted)
	run.stop(t)
	if r.get("c").Status != scheduler.StatusCancelled || r.outcome("c") != scheduler.ClassCancelled {
		t.Fatalf("status=%s outcome=%q", r.get("c").Status, r.outcome("c"))
	}
	if r.attempts("c") != 0 || r.runs() != 1 {
		t.Fatalf("attempts=%d runs=%d", r.attempts("c"), r.runs())
	}
}

// Cancelling a running job signals its dispatch once and the job ends
// cancelled, with the process gone.
func TestCancelRunningJobKillsIt(t *testing.T) {
	r := newRig(t)
	r.job("h", "hang")
	run := start(t, r.opts())
	r.waitStatus("h", scheduler.StatusRunning)
	var id string
	until(t, "the dispatch to be linked", func() bool { id = r.dispatchOf("h"); return id != "" })
	var pid int
	until(t, "a pid", func() bool {
		_ = r.raw.QueryRow(`SELECT COALESCE(pid,0) FROM dispatches WHERE id = ?`, id).Scan(&pid)
		return pid > 1
	})
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("the executor is not alive before the cancel: %v", err)
	}
	if _, err := r.store.CancelJob(context.Background(), "h", time.Now()); err != nil {
		t.Fatal(err)
	}
	r.waitStatus("h", scheduler.StatusCancelled)
	run.stop(t)
	if r.outcome("h") != scheduler.ClassCancelled {
		t.Fatalf("outcome %q", r.outcome("h"))
	}
	until(t, "the executor to be gone", func() bool { return syscall.Kill(pid, 0) != nil })
}

// Drain: a SIGTERM-style stop waits for a running job, claims nothing new,
// and ends with the runner row stopped.
func TestDrainWaitsForRunningJobsAndClaimsNothingNew(t *testing.T) {
	r := newRig(t)
	g := r.gateJob("g")
	run := start(t, r.opts())
	r.waitStatus("g", scheduler.StatusRunning)

	run.cancel()
	until(t, "the runner to report draining", func() bool { s, _ := r.runnerState(); return s == "draining" })
	r.job("late", "ok")
	select {
	case <-run.done:
		t.Fatal("the runner stopped while a job was still running")
	default:
	}
	release(t, g)
	select {
	case <-run.done:
	case <-time.After(waitFor):
		t.Fatal("the runner did not stop after the job finished")
	}
	if run.err != nil {
		t.Fatal(run.err)
	}
	if st, _ := r.runnerState(); st != "stopped" {
		t.Fatalf("runner state %q", st)
	}
	if r.get("g").Status != scheduler.StatusCompleted {
		t.Fatalf("g is %s", r.get("g").Status)
	}
	if r.get("late").Status != scheduler.StatusPending || r.attempts("late") != 0 {
		t.Fatalf("late job was claimed during the drain: %s, %d attempts", r.get("late").Status, r.attempts("late"))
	}

	// Negative control: a fresh runner claims the late job.
	again := start(t, r.opts())
	r.waitStatus("late", scheduler.StatusCompleted)
	again.stop(t)
}

// Drain timeout: the runner stops at the deadline and leaves the child
// running for recovery. It does not signal it.
func TestDrainTimeoutLeavesChildrenRunning(t *testing.T) {
	r := newRig(t)
	r.job("h", "hang")
	o := r.opts()
	o.DrainTimeout = 200 * time.Millisecond
	run := start(t, o)
	r.waitStatus("h", scheduler.StatusRunning)

	started := time.Now()
	run.cancel()
	select {
	case <-run.done:
	case <-time.After(waitFor):
		t.Fatal("the runner did not stop at the drain timeout")
	}
	if run.err != nil {
		t.Fatal(run.err)
	}
	if time.Since(started) < 150*time.Millisecond {
		t.Fatalf("stopped after %s, before the drain timeout", time.Since(started))
	}
	if st, _ := r.runnerState(); st != "stopped" {
		t.Fatalf("runner state %q", st)
	}
	var pid int
	if err := r.raw.QueryRow(`SELECT COALESCE(pid,0) FROM dispatches WHERE id = ?`, r.dispatchOf("h")).Scan(&pid); err != nil || pid <= 1 {
		t.Fatalf("pid %d, %v", pid, err)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("the child was signalled at the drain deadline: %v", err)
	}
	if r.get("h").Status != scheduler.StatusRunning {
		t.Fatalf("job is %s", r.get("h").Status)
	}
}

// A cancelling job whose executor ignores SIGTERM makes dispatch.Kill wait
// seconds before it escalates. That wait must not hold the main loop: the drain
// deadline is honoured, and the runner stops long before Kill would return.
func TestCancelOfStubbornChildDoesNotBlockDrainDeadline(t *testing.T) {
	r := newRig(t)
	r.job("s", "stubborn")
	o := r.opts()
	o.DrainTimeout = 200 * time.Millisecond
	run := start(t, o)
	r.waitStatus("s", scheduler.StatusRunning)
	var pid int
	until(t, "a pid", func() bool {
		_ = r.raw.QueryRow(`SELECT COALESCE(pid,0) FROM dispatches WHERE id = ?`, r.dispatchOf("s")).Scan(&pid)
		return pid > 1
	})
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	if _, err := r.store.CancelJob(context.Background(), "s", time.Now()); err != nil {
		t.Fatal(err)
	}
	// Wait until the slow kill is demonstrably in progress.
	until(t, "the kill to be in progress", func() bool { return goroutinesIn("terminateRecordedProcess") > 0 })

	started := time.Now()
	run.cancel()
	select {
	case <-run.done:
	case <-time.After(waitFor):
		t.Fatal("the runner did not stop")
	}
	if took := time.Since(started); took > 2*time.Second {
		t.Fatalf("drain took %s with a 200ms deadline: the cancel signal blocked the main loop", took)
	}
	if run.err != nil {
		t.Fatal(run.err)
	}
	// The signal still ends the child: SIGKILL follows the unanswered SIGTERM.
	until(t, "the stubborn child to be killed", func() bool { return syscall.Kill(pid, 0) != nil })
}

// The terminal-dispatch reaper ends with the runner. A child that outlives a
// drain timeout must not leave a goroutine polling a closed database.
func TestReaperEndsWithTheRunner(t *testing.T) {
	r := newRig(t)
	r.job("h", "hang")
	o := r.opts()
	o.DrainTimeout = 100 * time.Millisecond
	run := start(t, o)
	r.waitStatus("h", scheduler.StatusRunning)
	var pid int
	until(t, "a pid", func() bool {
		_ = r.raw.QueryRow(`SELECT COALESCE(pid,0) FROM dispatches WHERE id = ?`, r.dispatchOf("h")).Scan(&pid)
		return pid > 1
	})
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	until(t, "the reaper to be running", func() bool { return goroutinesIn("reapTerminalDispatch") > 0 })
	run.cancel()
	select {
	case <-run.done:
	case <-time.After(waitFor):
		t.Fatal("the runner did not stop")
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("the child is gone, so the test proves nothing: %v", err)
	}
	until(t, "the reaper goroutine to exit while its child still runs", func() bool {
		return goroutinesIn("reapTerminalDispatch") == 0
	})
}

// A reaper that is already waiting out a SIGTERM when the runner returns must
// not outlive it. The dispatch is terminal and its child ignores SIGTERM, so the
// reaper is mid-termination when the drain deadline passes. Run returns at the
// deadline, the reaper goroutine ends, and no SIGKILL follows from the runner:
// the child is left to the next runner's recovery.
func TestReaperTerminationEndsWithTheRunner(t *testing.T) {
	r := newRig(t)
	r.job("s", "stubborn")
	o := r.opts()
	o.DrainTimeout = 100 * time.Millisecond
	run := start(t, o)
	r.waitStatus("s", scheduler.StatusRunning)
	var id string
	until(t, "the dispatch to be linked", func() bool { id = r.dispatchOf("s"); return id != "" })
	var pid int
	until(t, "the child pid to be recorded", func() bool {
		return r.raw.QueryRow(`SELECT COALESCE(pid,0) FROM dispatches WHERE id = ?`, id).Scan(&pid) == nil && pid > 1
	})
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	// The stubborn executor writes <out>.started after it installed its SIGTERM
	// handler; a pid alone does not say the handler is in place.
	until(t, "the executor to ignore SIGTERM", func() bool {
		m, _ := filepath.Glob(filepath.Join(r.dir, "out-s*.started"))
		return len(m) > 0
	})
	if err := dispatch.Collect(context.Background(), dispatch.New(r.raw, nil), id); err != nil {
		t.Fatal(err)
	}
	until(t, "the reaper to be waiting out SIGTERM", func() bool { return goroutinesIn("terminateRecordedProcessUntil") > 0 })

	started := time.Now()
	run.cancel()
	select {
	case <-run.done:
	case <-time.After(waitFor):
		t.Fatal("the runner did not stop")
	}
	if took := time.Since(started); took > 2*time.Second {
		t.Fatalf("drain took %s with a 100ms deadline", took)
	}
	// Run joined the reaper: nothing of it is left the moment Run has returned.
	if n := goroutinesIn("reapTerminalDispatch") + goroutinesIn("terminateRecordedProcessUntil"); n != 0 {
		t.Fatalf("%d reaper goroutines still running after Run returned", n)
	}
	// The old code kept waiting and SIGKILLed the child about five seconds after
	// SIGTERM. Wait past that point: the runner must not have escalated.
	time.Sleep(6 * time.Second)
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("the child was killed after the runner returned: %v", err)
	}
}

func goroutinesIn(name string) int {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	n := 0
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, name) {
			n++
		}
	}
	return n
}
