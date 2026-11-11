//go:build icsched_test && linux

package runner

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/mistakeknot/intercore/internal/dispatch"
	"github.com/mistakeknot/intercore/internal/scheduler"
)

// Recovery tests (slice 4b). Two runners cannot live in one process (the lock
// conflict), so epoch 1 is a real runner that is started and stopped to record
// its identity and bump the epoch; the rows an epoch-1 runner would have left
// behind are then written through the store with Claim{Epoch: 1}, which the
// epoch guard accepts because the singleton row still says 1. The epoch-2 runner
// that follows is the one under test, and a hook snapshots the database after
// its startup recovery pass and before its first claim.

// snap is the state of one job as a test sees it.
type snap struct {
	Status   string
	Outcome  string
	Crashes  int
	Blocked  string
	Token    string
	Dispatch string
}

func (r *rig) snap(id string) snap {
	r.t.Helper()
	var s snap
	var oc, rb, tok, did sql.NullString
	if err := r.raw.QueryRow(`SELECT status, outcome_class, crash_requeues, retry_blocked, claim_token, dispatch_id FROM scheduler_jobs WHERE id = ?`, id).
		Scan(&s.Status, &oc, &s.Crashes, &rb, &tok, &did); err != nil {
		r.t.Fatal(err)
	}
	s.Outcome, s.Blocked, s.Token, s.Dispatch = oc.String, rb.String, tok.String, did.String
	return s
}

// firstEpoch records epoch 1 under the given boot id and returns with no runner
// alive.
func (r *rig) firstEpoch(o Options, boot string) int64 {
	r.t.Helper()
	if boot != "" {
		o.hooks.bootID = func() string { return boot }
	}
	run := start(r.t, o)
	run.stop(r.t)
	return run.epoch
}

func (r *rig) claim(id string, epoch int64) *scheduler.Claim {
	r.t.Helper()
	r.job(id, "ok")
	c, err := r.store.Claim(context.Background(), scheduler.ClaimParams{JobID: id, Epoch: epoch, Now: time.Now()})
	if err != nil {
		r.t.Fatalf("claim %s: %v", id, err)
	}
	return c
}

func (r *rig) launchAs(c *scheduler.Claim, result string) {
	r.t.Helper()
	if ok, err := r.store.RecordLaunchResult(context.Background(), c, result); err != nil || !ok {
		r.t.Fatalf("launch result %s: %v %v", result, ok, err)
	}
}

// linked creates a dispatch with the given status and pid and links it.
func (r *rig) linked(c *scheduler.Claim, status string, pid int) string {
	r.t.Helper()
	ctx := context.Background()
	did, err := dispatch.New(r.raw, nil).Create(ctx, &dispatch.Dispatch{AgentType: "codex", ProjectDir: r.dir})
	if err != nil {
		r.t.Fatal(err)
	}
	if status != "spawned" || pid != 0 {
		var p any
		if pid != 0 {
			p = pid
		}
		if _, err := r.raw.Exec(`UPDATE dispatches SET status = ?, pid = ? WHERE id = ?`, status, p, did); err != nil {
			r.t.Fatal(err)
		}
	}
	if err := r.store.Link(ctx, c, did, time.Now()); err != nil {
		r.t.Fatal(err)
	}
	return did
}

func (r *rig) paths(c *scheduler.Claim, containment, cgroup string) {
	r.t.Helper()
	out := filepath.Join(r.dir, "out-"+c.JobID)
	if ok, err := r.store.SetAttemptPaths(context.Background(), c, out, containment, cgroup); err != nil || !ok {
		r.t.Fatalf("attempt paths: %v %v", ok, err)
	}
}

func (r *rig) dispatchRow(did string) (status, class string, orphans int) {
	r.t.Helper()
	var c sql.NullString
	var n sql.NullInt64
	if err := r.raw.QueryRow(`SELECT status, quarantine_reason FROM dispatches WHERE id = ?`, did).Scan(&status, &c); err != nil {
		r.t.Fatalf("dispatch %s: %v", did, err)
	}
	if err := r.raw.QueryRow(`SELECT COALESCE(SUM(orphans_killed), 0) FROM dispatch_terminals WHERE dispatch_id = ?`, did).Scan(&n); err != nil {
		r.t.Fatalf("terminal of %s: %v", did, err)
	}
	return status, c.String, int(n.Int64)
}

// recovered starts the epoch-2 runner, waits for its startup recovery pass,
// reads the state through read (still before the first claim) and stops it.
// The runner's log is returned.
func (r *rig) recovered(o Options, boot string, read func()) string {
	r.t.Helper()
	lg := &logBuf{}
	o.Stderr = lg
	var once sync.Once
	passed := make(chan struct{})
	o.hooks.recoveryPass = func() {
		once.Do(func() {
			read()
			close(passed)
		})
	}
	if boot != "" {
		o.hooks.bootID = func() string { return boot }
	}
	run := start(r.t, o)
	select {
	case <-passed:
	case <-time.After(waitFor):
		r.t.Fatal("the startup recovery pass never ran")
	}
	run.stop(r.t)
	return lg.String()
}

// sleeper starts a long sleep in its own session and returns its pid.
func sleeper(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "120")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return cmd.Process.Pid
}

func alive(pid int) bool { return !gone(pid) }

// Rows 2 to 5 and 9 of the table, and the crash cap of row 3: the unlinked
// attempts of an earlier epoch are resolved by the startup pass, before the
// first claim, and every outcome is the table's.
func TestRecoveryUnlinkedRows(t *testing.T) {
	cases := []struct {
		name   string
		seed   func(r *rig, c *scheduler.Claim)
		status string
		class  string
		crash  int
		block  string
	}{
		{"row2 cancelling", func(r *rig, c *scheduler.Claim) { r.cancel(c.JobID) }, "cancelled", "cancelled", 0, ""},
		{"row3 launching no result", func(r *rig, c *scheduler.Claim) {}, "pending", "", 1, ""},
		{"row4 spawn_error", func(r *rig, c *scheduler.Claim) { r.launchAs(c, scheduler.LaunchSpawnError) }, "failed", "prestart", 0, ""},
		{"row4 rejected", func(r *rig, c *scheduler.Claim) { r.launchAs(c, scheduler.LaunchRejected) }, "failed", "prestart", 0, ""},
		{"row4 hook_aborted", func(r *rig, c *scheduler.Claim) { r.launchAs(c, scheduler.LaunchHookAborted) }, "failed", "prestart", 0, ""},
		{"row5 started but never linked", func(r *rig, c *scheduler.Claim) { r.launchAs(c, scheduler.LaunchStarted) }, "quarantined", scheduler.ReasonRunnerInconsistent, 0, ""},
		{"row9 running but never linked", func(r *rig, c *scheduler.Claim) {
			if _, err := r.raw.Exec(`UPDATE scheduler_jobs SET status = 'running' WHERE id = ?`, c.JobID); err != nil {
				t.Fatal(err)
			}
		}, "quarantined", scheduler.ReasonRunnerInconsistent, 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			o := r.opts()
			ep := r.firstEpoch(o, "")
			c := r.claim("x", ep)
			tc.seed(r, c)
			var got snap
			r.recovered(o, "", func() { got = r.snap("x") })
			if got.Status != tc.status || got.Outcome != tc.class || got.Crashes != tc.crash || got.Blocked != tc.block {
				t.Fatalf("after the startup pass %+v, want status=%s outcome=%q crashes=%d blocked=%q", got, tc.status, tc.class, tc.crash, tc.block)
			}
			if r.runs() != 0 && tc.status != "pending" {
				t.Fatalf("the executor ran %d times", r.runs())
			}
		})
	}
}

// R1c at the table level: the fourth crash before launch is abandoned, the
// counter stays at the cap (3 requeues, then abandoned).
func TestRecoveryCrashCapAbandons(t *testing.T) {
	r := newRig(t)
	o := r.opts()
	r.firstEpoch(o, "")
	r.job("x", "ok")
	for want := 1; want <= scheduler.MaxCrashRequeues+1; want++ {
		ep, err := r.store.CurrentEpoch(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.raw.Exec(`UPDATE scheduler_jobs SET not_before = 0 WHERE id = 'x'`); err != nil {
			t.Fatal(err)
		}
		if _, err := r.store.Claim(context.Background(), scheduler.ClaimParams{JobID: "x", Epoch: ep, Now: time.Now()}); err != nil {
			t.Fatalf("claim %d: %v", want, err)
		}
		var got snap
		r.recovered(o, "", func() {
			got = r.snap("x")
			// Keep this epoch's runner from claiming the requeued row.
			if _, err := r.raw.Exec(`UPDATE scheduler_jobs SET not_before = ? WHERE id = 'x'`, time.Now().Add(time.Hour).Unix()); err != nil {
				t.Error(err)
			}
		})
		if want <= scheduler.MaxCrashRequeues {
			if got.Status != "pending" || got.Crashes != want {
				t.Fatalf("crash %d: %+v, want pending with crash_requeues=%d", want, got, want)
			}
			continue
		}
		if got.Status != "failed" || got.Outcome != scheduler.ClassAbandoned || got.Crashes != scheduler.MaxCrashRequeues {
			t.Fatalf("crash %d: %+v, want failed/abandoned with crash_requeues=%d", want, got, scheduler.MaxCrashRequeues)
		}
	}
	if r.runs() != 0 {
		t.Fatalf("the executor ran %d times", r.runs())
	}
}

// Row 6: the linked dispatch is terminal, or was pruned. The shared pipeline
// finalizes the job without a Collect; a pruned dispatch is indeterminate.
func TestRecoveryTerminalAndPrunedDispatch(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  string
		prune   bool
		outcome string
	}{
		{"completed dispatch", "completed", false, ""},
		{"failed dispatch", "failed", false, ""},
		{"pruned dispatch", "", true, scheduler.ClassIndeterminate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			o := r.opts()
			ep := r.firstEpoch(o, "")
			c := r.claim("x", ep)
			r.launchAs(c, scheduler.LaunchStarted)
			st := tc.status
			if st == "" {
				st = "spawned"
			}
			did := r.linked(c, st, 0)
			r.paths(c, "none", "")
			if tc.prune {
				// A prune runs outside the foreign-key checks of this connection.
				conn, err := r.raw.Conn(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				for _, q := range []string{`PRAGMA foreign_keys = OFF`, `DELETE FROM dispatches WHERE id = '` + did + `'`, `PRAGMA foreign_keys = ON`} {
					if _, err := conn.ExecContext(context.Background(), q); err != nil {
						t.Fatal(err)
					}
				}
				conn.Close()
			}
			var got snap
			r.recovered(o, "", func() { got = r.snap("x") })
			if got.Status == "running" || got.Status == "launching" || got.Token != "" {
				t.Fatalf("the row was not finalized: %+v", got)
			}
			if tc.outcome != "" && got.Outcome != tc.outcome {
				t.Fatalf("outcome %q, want %q", got.Outcome, tc.outcome)
			}
			if tc.outcome == "" && got.Outcome == "" {
				t.Fatalf("no outcome class: %+v", got)
			}
			if r.runs() != 0 {
				t.Fatalf("the executor ran %d times", r.runs())
			}
		})
	}
}

// Row 7 with a recorded PID but no recorded identity has no proof the process
// is the one the dead runner started: it is logged as proof_absent, nothing is
// signalled and the row stays in flight (and keeps its claim), pass after pass.
func TestRecoveryRecordedPIDWithoutIdentityIsReChecked(t *testing.T) {
	r := newRig(t)
	o := r.opts()
	o.RecoverEvery = 20 * time.Millisecond
	ep := r.firstEpoch(o, "boot-A")
	c := r.claim("x", ep)
	r.launchAs(c, scheduler.LaunchStarted)
	pid := sleeper(t)
	did := r.linked(c, "running", pid)
	r.paths(c, "none", "")
	if err := r.store.MarkRunning(context.Background(), c, time.Now()); err != nil {
		t.Fatal(err)
	}

	var passes atomic.Int32
	o.hooks.recoveryPass = func() { passes.Add(1) }
	o.hooks.bootID = func() string { return "boot-A" } // same boot: no shortcut
	lg := &logBuf{}
	o.Stderr = lg
	run := start(t, o)
	until(t, "three recovery passes", func() bool { return passes.Load() >= 3 })
	run.stop(t)

	got := r.snap("x")
	if got.Status != "running" || got.Token != c.Token || got.Dispatch != did {
		t.Fatalf("the row left flight: %+v", got)
	}
	if !alive(pid) {
		t.Fatal("the recorded process was signalled")
	}
	if !strings.Contains(lg.String(), "recorded process: proof_absent") {
		t.Fatalf("no proof_absent line in the log:\n%s", lg.String())
	}
}

// The boot change shortcut: the previous runner recorded another boot_id, so
// none of its processes exists. The dispatch is terminalized by reconcile with
// no process lookup (the live sleeper proves nothing was signalled) and the row
// finalizes indeterminate.
func TestRecoveryBootChangeShortcut(t *testing.T) {
	r := newRig(t)
	o := r.opts()
	ep := r.firstEpoch(o, "boot-A")
	c := r.claim("x", ep)
	r.launchAs(c, scheduler.LaunchStarted)
	pid := sleeper(t)
	did := r.linked(c, "running", pid)
	r.paths(c, "none", "")
	if err := r.store.MarkRunning(context.Background(), c, time.Now()); err != nil {
		t.Fatal(err)
	}
	var got snap
	r.recovered(o, "boot-B", func() { got = r.snap("x") })
	if got.Status != "failed" && got.Status != "quarantined" {
		t.Fatalf("not finalized: %+v", got)
	}
	if got.Outcome != scheduler.ClassIndeterminate {
		t.Fatalf("outcome %q, want indeterminate", got.Outcome)
	}
	if got.Blocked != "" {
		t.Fatalf("retry_blocked %q: a rebooted host has no survivors", got.Blocked)
	}
	st, class, _ := r.dispatchRow(did)
	if st != "failed" || class != dispatch.WorkerOutcomeIndeterminate {
		t.Fatalf("dispatch status=%q class=%q, want failed/%s", st, class, dispatch.WorkerOutcomeIndeterminate)
	}
	if !alive(pid) {
		t.Fatal("the shortcut signalled a process")
	}
}

// An unreadable boot id on this runner is not a reboot: the shortcut would
// skip the survivor proof after an ordinary same-boot crash.
func TestRecoveryUnknownBootIDIsNotAReboot(t *testing.T) {
	r := newRig(t)
	o := r.opts()
	ep := r.firstEpoch(o, "boot-A")
	c := r.claim("x", ep)
	r.launchAs(c, scheduler.LaunchStarted)
	pid := sleeper(t)
	did := r.linked(c, "running", pid)
	r.paths(c, "none", "")
	if err := r.store.MarkRunning(context.Background(), c, time.Now()); err != nil {
		t.Fatal(err)
	}
	o.hooks.bootID = func() string { return "" }
	var got snap
	r.recovered(o, "", func() { got = r.snap("x") })
	if got.Status != "running" || got.Token != c.Token {
		t.Fatalf("job %+v, want left running under its claim", got)
	}
	if st, _, _ := r.dispatchRow(did); st != "running" {
		t.Fatalf("dispatch %q, want untouched", st)
	}
	if !alive(pid) {
		t.Fatal("a process was signalled")
	}
}

// Row 8 with no recorded PID and a fallback attempt (no cgroup), and the legacy
// row, are left for slice 4d: logged, untouched.
func TestRecoveryFallbackAndLegacyRowsAreSkipped(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "fallback row 8"
		if legacy {
			name = "legacy row 1"
		}
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			o := r.opts()
			ep := r.firstEpoch(o, "boot-A")
			c := r.claim("x", ep)
			r.launchAs(c, scheduler.LaunchStarted)
			r.linked(c, "spawned", 0)
			r.paths(c, "none", "")
			if legacy {
				if _, err := r.raw.Exec(`UPDATE scheduler_job_attempts SET legacy = 1 WHERE claim_token = ?`, c.Token); err != nil {
					t.Fatal(err)
				}
			}
			var got snap
			lg := r.recovered(o, "boot-A", func() { got = r.snap("x") })
			if got.Status != "launching" || got.Token != c.Token {
				t.Fatalf("the row left flight: %+v", got)
			}
			want := "fallback-mode survivor sweep is not part of this runner yet"
			if legacy {
				want = "legacy rule is not part of this runner yet"
			}
			if !strings.Contains(lg, want) {
				t.Fatalf("no skip line %q in the log:\n%s", want, lg)
			}
		})
	}
}

// Row 8 in cgroup mode (P3c): a dispatch with no recorded PID whose attempt
// cgroup still holds a process. The cgroup is killed and proven empty, the
// dispatch is terminalized indeterminate with orphans_killed, and the job
// finalizes indeterminate with no retry_blocked (the cgroup is the proof).
func TestRecoverySurvivorInCgroupIsKilledAndProven(t *testing.T) {
	r, o := containRig(t)
	row, err := r.store.ReadRunner(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ep := r.firstEpoch(o, "boot-A")
	m := &cgroupMgr{root: o.CgroupDir}
	m.setQueue(row.QueueID)
	c := r.claim("x", ep)
	p, f, err := m.create("x", c.AttemptNo)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sleep", "120")
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(f.Fd())}
	if err := cmd.Start(); err != nil {
		f.Close()
		t.Skipf("cannot start a process into %s: %v", p, err)
	}
	f.Close()
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	r.launchAs(c, scheduler.LaunchStarted)
	did := r.linked(c, "spawned", 0)
	r.paths(c, "cgroup", p)

	var got snap
	var popAtPass bool
	lg := r.recovered(o, "boot-A", func() {
		got = r.snap("x")
		popAtPass, _ = populated(p)
	})
	_ = lg
	select {
	case <-exited:
	default:
		t.Fatal("the survivor in the attempt cgroup is still alive")
	}
	if popAtPass {
		t.Fatal("the cgroup was not proven empty by the startup pass")
	}
	if got.Outcome != scheduler.ClassIndeterminate || got.Blocked != "" || got.Token != "" {
		t.Fatalf("job %+v, want finalized indeterminate with no retry_blocked", got)
	}
	st, class, orphans := r.dispatchRow(did)
	if st != "failed" || class != dispatch.WorkerOutcomeIndeterminate || orphans != 1 {
		t.Fatalf("dispatch status=%q class=%q orphans_killed=%d", st, class, orphans)
	}
	if _, err := os.Stat(p); err == nil {
		t.Fatalf("the proven-empty attempt cgroup %s was not removed", p)
	}
}

// The recorded cgroup path is data: when it is not the path this runner derives
// for the attempt (a different --cgroup-dir, or a damaged value) nothing is
// signalled and the row is quarantined runner_inconsistent.
func TestRecoveryCgroupPathMismatchQuarantinesWithoutSignal(t *testing.T) {
	mismatchCase(t, "")
}

// A cancelling job with a mismatched cgroup path must not be finished as
// cancelled: the store would turn the quarantine into a closed cancel while
// the worker may still run. The row stays in flight and nothing is signalled.
func TestRecoveryCgroupPathMismatchKeepsCancellingInFlight(t *testing.T) {
	mismatchCase(t, "before")
}

// The cancel commits after recovery read the job and before the store acts:
// the refusal must come from inside the store transaction.
func TestRecoveryCgroupPathMismatchCancelRacingTheQuarantine(t *testing.T) {
	mismatchCase(t, "during")
}

func mismatchCase(t *testing.T, cancel string) {
	cancelling := cancel != ""
	r, o := containRig(t)
	ep := r.firstEpoch(o, "boot-A")
	c := r.claim("x", ep)
	foreign := filepath.Join(o.CgroupDir, "attempt-foreign-1")
	if err := os.Mkdir(foreign, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(foreign, os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sleep", "120")
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(f.Fd())}
	if err := cmd.Start(); err != nil {
		f.Close()
		t.Skipf("cannot start a process into %s: %v", foreign, err)
	}
	f.Close()
	gone := make(chan struct{})
	go func() { _ = cmd.Wait(); close(gone) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-gone
		for i := 0; i < 100; i++ {
			if os.Remove(foreign) == nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	})

	r.launchAs(c, scheduler.LaunchStarted)
	r.linked(c, "spawned", 0)
	r.paths(c, "cgroup", foreign)
	switch cancel {
	case "before":
		if _, err := r.store.CancelJob(context.Background(), "x", time.Now()); err != nil {
			t.Fatal(err)
		}
	case "during":
		var once sync.Once
		o.hooks.recoverRow = func(token, step string) {
			if strings.HasPrefix(step, "row") {
				once.Do(func() {
					if _, err := r.store.CancelJob(context.Background(), "x", time.Now()); err != nil {
						t.Error(err)
					}
				})
			}
		}
	}

	var got snap
	r.recovered(o, "boot-A", func() { got = r.snap("x") })
	if cancelling {
		if got.Status != "cancelling" || got.Token != c.Token {
			t.Fatalf("job %+v, want cancelling and still claimed by %s", got, c.Token)
		}
	} else if got.Status != "quarantined" || got.Outcome != scheduler.ReasonRunnerInconsistent {
		t.Fatalf("job %+v, want quarantined runner_inconsistent", got)
	}
	if !alive(cmd.Process.Pid) {
		t.Fatal("a process in a cgroup this runner did not derive was killed")
	}
}

// L1: a launch held at after_claim while recovery passes run is not touched:
// the current epoch's rows are out of the pass's scope, so the row is not
// requeued, crash_requeues stays 0 and the executor runs once.
func TestRecoveryPassLeavesCurrentEpochRowsAlone(t *testing.T) {
	r := newRig(t)
	o := r.opts()
	o.RecoverEvery = 10 * time.Millisecond
	r.job("x", "ok")
	hold := make(chan struct{})
	var once sync.Once
	reached := make(chan struct{})
	o.hooks.barrier = func(point, token string) {
		if point == "after_claim" {
			once.Do(func() { close(reached); <-hold })
		}
	}
	var passes atomic.Int32
	o.hooks.recoveryPass = func() { passes.Add(1) }
	run := start(t, o)
	select {
	case <-reached:
	case <-time.After(waitFor):
		t.Fatal("never reached after_claim")
	}
	base := passes.Load()
	until(t, "five recovery passes while the launch is held", func() bool { return passes.Load() >= base+5 })
	got := r.snap("x")
	if got.Status != "launching" || got.Crashes != 0 || got.Token == "" {
		t.Fatalf("held row %+v was touched by recovery", got)
	}
	close(hold)
	r.waitStatus("x", scheduler.StatusCompleted)
	run.stop(t)
	if got := r.snap("x"); got.Crashes != 0 {
		t.Fatalf("crash_requeues %d, want 0", got.Crashes)
	}
	if r.runs() != 1 {
		t.Fatalf("the executor ran %d times, want 1", r.runs())
	}
}

// A recovery pass with a failing step backs the row off and leaves it for the
// next pass instead of dropping it.
func TestRecoveryErrorKeepsRowInFlight(t *testing.T) {
	r := newRig(t)
	o := r.opts()
	o.RecoverEvery = 10 * time.Millisecond
	ep := r.firstEpoch(o, "")
	c := r.claim("x", ep)
	r.launchAs(c, scheduler.LaunchStarted)
	r.linked(c, "completed", 0)
	r.paths(c, "none", "")
	var fails atomic.Int32
	o.hooks.inject = func(kind string) error {
		if kind == "finalize" && fails.Add(1) <= 2 {
			return fmt.Errorf("injected SQLITE_BUSY (finalize)")
		}
		return nil
	}
	var passes atomic.Int32
	o.hooks.recoveryPass = func() { passes.Add(1) }
	run := start(t, o)
	until(t, "the row to finalize after the injected failures", func() bool { return r.snap("x").Token == "" })
	run.stop(t)
	if fails.Load() < 3 {
		t.Fatalf("finalize was tried %d times, want at least 3 (two injected failures and the success)", fails.Load())
	}
}
