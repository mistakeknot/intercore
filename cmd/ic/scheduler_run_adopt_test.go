//go:build icsched_test && linux

package main

// Recorded-PID recovery and adoption through the real icsched_test binary
// (plan section 9, slice 4c): the cgroup-mode kill matrix rows for barriers 5
// to 7 and 11, the adopted path of the pipeline barriers, K6, E2d and E2e
// (adopted halves), P1, P5 and P6. Each runner runs under strace, so the launch
// count is observed from outside the process under test. A "gate" job runs
// until the test creates its gate file, so a worker can be kept alive across a
// runner kill and finished on demand.

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/mistakeknot/intercore/internal/db"
	"github.com/mistakeknot/intercore/internal/scheduler"
)

// addJobWith is addJob with a hook on the job before it is stored.
func (e *env) addJobWith(path, id, mode string, mut func(*scheduler.SpawnJob)) {
	e.t.Helper()
	prompt := filepath.Join(e.cwd, id+".prompt")
	if err := os.WriteFile(prompt, []byte(mode+"\n"), 0o600); err != nil {
		e.t.Fatal(err)
	}
	so, _ := json.Marshal(map[string]any{"AgentType": "codex", "PromptFile": prompt})
	d, err := db.Open(path, 5*time.Second)
	if err != nil {
		e.t.Fatal(err)
	}
	defer d.Close()
	j := &scheduler.SpawnJob{
		ID: id, Status: scheduler.StatusPending, Priority: scheduler.PriorityNormal, AgentType: "codex",
		SpawnOpts: string(so), CreatedAt: time.Unix(1_800_000_000, 0),
		ProjectDir: e.cwd, OutputBase: filepath.Join(e.cwd, "out-"+id),
	}
	if mut != nil {
		mut(j)
	}
	if err := scheduler.NewStore(d.SqlDB()).Create(context.Background(), j); err != nil {
		e.t.Fatalf("create %s: %v", id, err)
	}
}

// newRecNoCgroup is a recovery scenario in fallback mode: no delegated subtree
// and --no-containment, so nothing is contained and the recorded PID is the
// only handle on the worker.
func newRecNoCgroup(t *testing.T) *rec {
	t.Helper()
	strace := needStrace(t)
	e := newEnv(t)
	return &rec{t: t, e: e, strace: strace, fake: e.fakeScript(), path: e.newDB("q.db")}
}

// gateJob adds a job that runs until the returned release function is called
// (or the test ends, so a leftover worker does not outlive it).
func (r *rec) gateJob(id string, mut func(*scheduler.SpawnJob)) (release func()) {
	r.t.Helper()
	gate := filepath.Join(r.e.cwd, id+".gate")
	r.e.addJobWith(r.path, id, "gate "+gate, mut)
	rel := func() { _ = os.WriteFile(gate, nil, 0o600) }
	r.t.Cleanup(rel)
	return rel
}

func (r *rec) token(id string) string {
	return r.str(`SELECT claim_token FROM scheduler_jobs WHERE id = '` + id + `'`)
}

// dispatchPID is the PID the dispatch recorded.
func (r *rec) dispatchPID() int {
	n, _ := strconv.Atoi(r.str(`SELECT pid FROM dispatches LIMIT 1`))
	return n
}

// waitAdopted waits until the job carries a claim token other than old: the
// restarted runner took it over.
func (r *rec) waitAdopted(id, old string) string {
	r.t.Helper()
	deadline := time.Now().Add(waitLimit)
	for {
		if tk := r.token(id); tk != "" && tk != old {
			return tk
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("job %s was not adopted (token still %s)", id, old)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// emptyAttemptCgroup kills everything in the job's attempt cgroup, like a
// worker that died with the runner.
func (r *rec) emptyAttemptCgroup(id string) {
	r.t.Helper()
	dir := r.attemptDir(id)
	if dir == "" {
		r.t.Fatal("no attempt cgroup recorded")
	}
	_ = os.WriteFile(filepath.Join(dir, "cgroup.kill"), []byte("1"), 0o644)
	for i := 0; i < 500 && populatedDir(dir); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if populatedDir(dir) {
		r.t.Fatalf("cannot empty %s", dir)
	}
}

func (r *rec) waitLog(p *proc, needle string) {
	r.t.Helper()
	deadline := time.Now().Add(waitLimit)
	for !strings.Contains(p.stderr(), needle) {
		if time.Now().After(deadline) {
			r.t.Fatalf("the runner never logged %q: %s", needle, p.stderr())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (r *rec) noSecondAttempt(want int) {
	r.t.Helper()
	r.e.heartbeats(r.path, 2)
	if got := r.e.count(r.path, `SELECT COUNT(*) FROM scheduler_job_attempts WHERE job_id = 'j1'`); got != want {
		r.t.Fatalf("attempts = %d, want %d", got, want)
	}
	if got := r.e.count(r.path, `SELECT COUNT(*) FROM dispatch_terminals`); got > 1 {
		r.t.Fatalf("terminal rows = %d, want at most 1", got)
	}
}

// The kill matrix rows that need the recorded-PID check: a runner killed after
// the PID was recorded, with the worker alive (adopted) or dead.
func TestCLIRecoveryRecordedPIDMatrix(t *testing.T) {
	type tc struct {
		name    string
		point   string
		mode    string // "gate" or a fake executor mode
		kill    bool   // the worker dies with the runner
		status  string
		outcome string
		adopt   bool
		log     string // the recorded-process verdict the restarted runner logs
		check   func(r *rec)
	}
	cases := []tc{
		{name: "b5_alive", point: "after_identity_before_running_cas", mode: "gate", status: "completed", outcome: "ok", adopt: true, log: "recorded process: alive_match"},
		{name: "b5_dead", point: "after_identity_before_running_cas", mode: "gate", kill: true, status: "failed", outcome: "indeterminate", log: "recorded process: dead"},
		{name: "b11_started_alive", point: "after_launch_result_commit", mode: "gate", status: "completed", outcome: "ok", adopt: true, log: "recorded process: alive_match"},
		{name: "b11_started_dead", point: "after_launch_result_commit", mode: "gate", kill: true, status: "failed", outcome: "indeterminate", log: "recorded process: dead"},
		// The wrapper exited and left a descendant: recovery kills the attempt
		// cgroup and proves it empty before it reads any evidence.
		{name: "b6_descendant_alive", point: "after_exit_before_evidence", mode: "spawn-child", status: "completed", outcome: "ok",
			check: func(r *rec) {
				child := r.recordedChild()
				if child <= 0 {
					r.t.Fatalf("no descendant pid recorded")
				}
				waitProcGone(r.t, child, "the executor's descendant")
			}},
		{name: "b7_ok", point: "after_evidence_before_collect", mode: "ok", status: "completed", outcome: "ok"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			r := newRec(t)
			var release func()
			if c.mode == "gate" {
				release = r.gateJob("j1", nil)
			} else {
				r.e.addJob(r.path, "j1", c.mode)
			}
			r.crashAt(c.point, "")
			if c.mode == "spawn-child" {
				for i := 0; i < 500 && r.liveChild() == 0; i++ {
					time.Sleep(10 * time.Millisecond)
				}
				if r.liveChild() == 0 {
					t.Fatal("the executor never started a live descendant")
				}
			}
			pid := r.dispatchPID()
			if c.mode == "gate" && (pid <= 1 || procGone(pid)) {
				t.Fatalf("recorded pid %d is not a live worker at the barrier", pid)
			}
			old := r.token("j1")
			if c.kill {
				r.emptyAttemptCgroup("j1")
				waitProcGone(t, pid, "the worker")
			}
			p := r.run(nil)
			p.waitReady(t)
			if c.adopt {
				if tk := r.waitAdopted("j1", old); tk == old {
					t.Fatal("adopted with the old token")
				}
				if r.e.count(r.path, `SELECT COUNT(*) FROM scheduler_job_attempts WHERE job_id = 'j1' AND claim_token = '`+old+`'`) != 0 {
					t.Fatal("the attempt row still carries the old token")
				}
				if procGone(pid) {
					t.Fatal("adoption signalled the worker")
				}
				release()
			}
			status, outcome := r.waitOutcome("j1")
			if status != c.status || outcome != c.outcome {
				t.Fatalf("job is %s/%s, want %s/%s", status, outcome, c.status, c.outcome)
			}
			if c.log != "" && !strings.Contains(p.stderr(), c.log) {
				t.Fatalf("the runner did not log %q: %s", c.log, p.stderr())
			}
			if got := r.crashes("j1"); got != 0 {
				t.Fatalf("crash_requeues = %d, want 0", got)
			}
			if n := r.execs(); n != 1 {
				t.Fatalf("execve of the executor = %d, want 1", n)
			}
			r.noSecondAttempt(1)
			if dir := r.attemptDir("j1"); dir != "" && populatedDir(dir) {
				t.Fatalf("attempt cgroup %s is not empty", dir)
			}
			if c.check != nil {
				c.check(r)
			}
			r.stop(p)
		})
	}
}

// Barrier 7: Collect and classify from the stored evidence give the class an
// uninterrupted run gives.
func TestCLIRecoveryBarrier7MatchesUninterrupted(t *testing.T) {
	for _, mode := range []string{"ok", "receipt quota_exhausted", "noverdict", "fail"} {
		mode := mode
		t.Run(strings.ReplaceAll(mode, " ", "_"), func(t *testing.T) {
			ctl := newRec(t)
			ctl.e.addJob(ctl.path, "j1", mode)
			p := ctl.run(nil)
			p.waitReady(t)
			wantS, wantO := ctl.waitOutcome("j1")
			ctl.stop(p)

			r := newRec(t)
			r.e.addJob(r.path, "j1", mode)
			r.crashAt("after_evidence_before_collect", "")
			p2 := r.run(nil)
			p2.waitReady(t)
			gotS, gotO := r.waitOutcome("j1")
			if gotS != wantS || gotO != wantO {
				t.Fatalf("recovered %s/%s, an uninterrupted run gives %s/%s", gotS, gotO, wantS, wantO)
			}
			if n := r.execs(); n != 1 {
				t.Fatalf("execve of the executor = %d, want 1", n)
			}
			r.stop(p2)
		})
	}
}

// The pipeline barriers 6 to 9 on the adopted path: barrier 5 with the worker
// alive, a restart that adopts it, the adopted worker exits and the adopting
// runner is killed at the barrier; a third runner finishes the job. Every path
// gives the same class and one launch.
func TestCLIRecoveryAdoptedPipelineBarriers(t *testing.T) {
	for _, point := range []string{"after_exit_before_evidence", "after_evidence_before_collect", "after_collect_before_finalize", "in_finalize_before_commit"} {
		point := point
		t.Run(point, func(t *testing.T) {
			r := newRec(t)
			release := r.gateJob("j1", nil)
			r.crashAt("after_identity_before_running_cas", "")
			old := r.token("j1")
			bar := newBarrier(t, t.TempDir())
			p := r.run(bar.env(point))
			p.waitReady(t)
			r.waitAdopted("j1", old)
			release()
			bar.wait(point)
			r.killHard(p)
			if _, outcome := r.e.jobOutcome(r.path, "j1"); outcome != "" && point != "in_finalize_before_commit" {
				t.Fatalf("the job finished at %s: outcome %s", point, outcome)
			}
			p3 := r.run(nil)
			p3.waitReady(t)
			status, outcome := r.waitOutcome("j1")
			if status != "completed" || outcome != "ok" {
				t.Fatalf("job is %s/%s, want completed/ok", status, outcome)
			}
			if got := r.crashes("j1"); got != 0 {
				t.Fatalf("crash_requeues = %d", got)
			}
			if n := r.execs(); n != 1 {
				t.Fatalf("execve of the executor = %d, want 1", n)
			}
			r.noSecondAttempt(1)
			r.stop(p3)
		})
	}
}

// K6: kill -9 mid-run with the worker killed too, under --idempotent
// --retry-on indeterminate --max-attempts 2, gives exactly two launches: the
// first attempt is proven dead and retried once.
func TestCLIRecoveryK6IdempotentRetryTwoLaunches(t *testing.T) {
	r := newRec(t)
	r.e.addJobWith(r.path, "j1", "sleep 2", func(j *scheduler.SpawnJob) {
		j.Idempotent = true
		j.RetryPolicy = `{"max_attempts":2,"backoff":{"base_s":0.05,"factor":1,"max_s":0.05,"jitter":0},"retry_on":["indeterminate"]}`
	})
	r.crashAt("after_identity_before_running_cas", "")
	pid := r.dispatchPID()
	r.emptyAttemptCgroup("j1")
	waitProcGone(t, pid, "the first attempt's worker")
	p := r.run(nil)
	p.waitReady(t)
	r.e.waitJob(r.path, "j1", "completed")
	if got := r.e.count(r.path, `SELECT COUNT(*) FROM scheduler_job_attempts WHERE job_id = 'j1'`); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
	if n := r.execs(); n != 2 {
		t.Fatalf("execve of the executor = %d, want 2", n)
	}
	r.stop(p)
}

// unreadableRoot is a procfs root whose entry for pid cannot be read: reading
// <root>/<pid>/stat fails because it is a directory.
func unreadableRoot(t *testing.T, pid int) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, strconv.Itoa(pid), "stat"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

// P5: the recorded PID cannot be read. Nothing happens for five minutes (the
// clock hook moves the rule's clock); then cgroup mode kills the attempt
// cgroup, proves it empty and finishes the job indeterminate, and fallback
// mode quarantines it survivor_unknown and signals nothing.
func TestCLIRecoveryUnreadablePIDAfterFiveMinutes(t *testing.T) {
	for _, mode := range []string{"cgroup", "fallback"} {
		mode := mode
		t.Run(mode, func(t *testing.T) {
			var r *rec
			if mode == "cgroup" {
				r = newRec(t)
			} else {
				r = newRecNoCgroup(t)
			}
			release := r.gateJob("j1", nil)
			r.crashAt("after_identity_before_running_cas", "")
			pid := r.dispatchPID()
			if pid <= 1 || procGone(pid) {
				t.Fatalf("recorded pid %d is not a live worker", pid)
			}
			old := r.token("j1")
			dir := t.TempDir()
			rootFile, clockFile := filepath.Join(dir, "procroot"), filepath.Join(dir, "clock")
			if err := os.WriteFile(rootFile, []byte(unreadableRoot(t, pid)), 0o600); err != nil {
				t.Fatal(err)
			}
			p := r.run([]string{"IC_SCHED_FAULT=procfs_root_file:" + rootFile + ",clock_offset_file:" + clockFile})
			p.waitReady(t)
			r.waitLog(p, "recorded process: unreadable")
			// Several repair passes inside the five minutes change nothing.
			time.Sleep(600 * time.Millisecond)
			if status, outcome := r.e.jobOutcome(r.path, "j1"); outcome != "" || status == "failed" || status == "quarantined" || status == "completed" {
				t.Fatalf("the row left flight inside five minutes: %s/%s", status, outcome)
			}
			if r.token("j1") != old {
				t.Fatal("an unreadable process was adopted")
			}
			if procGone(pid) {
				t.Fatal("the worker was signalled inside five minutes")
			}
			if err := os.WriteFile(clockFile, []byte(strconv.Itoa(5*60+1)), 0o600); err != nil {
				t.Fatal(err)
			}
			status, outcome := r.waitOutcome("j1")
			if mode == "cgroup" {
				if status != "failed" || outcome != "indeterminate" {
					t.Fatalf("job is %s/%s, want failed/indeterminate", status, outcome)
				}
				waitProcGone(t, pid, "the worker (its attempt cgroup was killed)")
				if d := r.attemptDir("j1"); d != "" && populatedDir(d) {
					t.Fatalf("attempt cgroup %s is not proven empty", d)
				}
			} else {
				if status != "quarantined" || outcome != "survivor_unknown" {
					t.Fatalf("job is %s/%s, want quarantined/survivor_unknown", status, outcome)
				}
				r.e.heartbeats(r.path, 2)
				if procGone(pid) {
					t.Fatal("fallback mode signalled the worker")
				}
				release()
				waitProcGone(t, pid, "the worker after its gate opened")
			}
			if n := r.execs(); n != 1 {
				t.Fatalf("execve of the executor = %d, want 1", n)
			}
			r.stop(p)
		})
	}
}

func becomeSubreaper(t *testing.T) {
	t.Helper()
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		t.Skipf("cannot become a subreaper: %v", err)
	}
	t.Cleanup(func() { _ = unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 0, 0, 0, 0) })
}

// procState is the state letter of pid, or 0 when it is not in /proc.
func procState(pid int) byte {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	s := string(b)
	i := strings.LastIndex(s, ") ")
	if i < 0 || len(s) <= i+2 {
		return 0
	}
	return s[i+2]
}

// P6: an adopted worker becomes an unreaped zombie: its parent is this test,
// the subreaper, which does not wait. The zombie counts as exited, the pipeline
// runs and the job is finalized once. The test records whether the zombie kept
// the attempt cgroup populated (slice 0 item 7): the pipeline's extinction step
// returned, so it did not.
func TestCLIRecoveryAdoptedZombieIsExited(t *testing.T) {
	becomeSubreaper(t)
	r := newRec(t)
	release := r.gateJob("j1", nil)
	r.crashAt("after_identity_before_running_cas", "")
	pid := r.dispatchPID()
	old := r.token("j1")
	p := r.run(nil)
	p.waitReady(t)
	r.waitAdopted("j1", old)
	dir := r.attemptDir("j1")
	release()
	status, outcome := r.waitOutcome("j1")
	if status != "completed" || outcome != "ok" {
		t.Fatalf("job is %s/%s, want completed/ok", status, outcome)
	}
	st := procState(pid)
	t.Logf("slice 0 item 7: at finalize the worker %d is in state %q; its attempt cgroup %s was proven empty and finalized with the zombie still unreaped (zombie holds the cgroup populated: no)", pid, string(st), dir)
	if st != 'Z' {
		t.Fatalf("worker %d is in state %q, want an unreaped zombie (Z)", pid, string(st))
	}
	if n := r.execs(); n != 1 {
		t.Fatalf("execve of the executor = %d, want 1", n)
	}
	r.noSecondAttempt(1)
	var ws syscall.WaitStatus
	_, _ = syscall.Wait4(pid, &ws, 0, nil)
	r.stop(p)
}

// E2d, adopted half: a healthy worker adopted by a restarted runner is left
// alone across at least four repair passes (100ms apart), then finishes.
func TestCLIRecoveryAdoptedHealthyWorkerUntouched(t *testing.T) {
	r := newRec(t)
	release := r.gateJob("j1", nil)
	r.crashAt("after_identity_before_running_cas", "")
	pid := r.dispatchPID()
	old := r.token("j1")
	p := r.run(nil)
	p.waitReady(t)
	tk := r.waitAdopted("j1", old)
	time.Sleep(1200 * time.Millisecond) // at least eight repair passes
	if procGone(pid) {
		t.Fatal("the adopted worker was signalled")
	}
	if r.token("j1") != tk {
		t.Fatal("the adopted claim changed under the repair passes")
	}
	if status, outcome := r.e.jobOutcome(r.path, "j1"); outcome != "" || status == "failed" || status == "quarantined" || status == "completed" {
		t.Fatalf("the adopted job left flight: %s/%s", status, outcome)
	}
	if got := r.e.count(r.path, `SELECT COUNT(*) FROM scheduler_job_attempts WHERE job_id = 'j1'`); got != 1 {
		t.Fatalf("attempts = %d, want 1", got)
	}
	release()
	if status, outcome := r.waitOutcome("j1"); status != "completed" || outcome != "ok" {
		t.Fatalf("job is %s/%s, want completed/ok", status, outcome)
	}
	if n := r.execs(); n != 1 {
		t.Fatalf("execve of the executor = %d, want 1", n)
	}
	r.stop(p)
}

// E2e, adopted half: the entry is dropped while the worker runs, so the runner
// exits 5 with no write after detection. The worker is reparented to this test
// (a subreaper that reaps). The restarted runner adopts it through the
// recorded-PID check, finalizes the job once and launches nothing.
func TestCLIRecoveryLostEntryWhileRunningAdopted(t *testing.T) {
	becomeSubreaper(t)
	r := newRec(t)
	release := r.gateJob("j1", nil)
	p, _ := r.heldAt("after_identity_before_running_cas", "drop_entry:after_identity_before_running_cas")
	rpid := r.e.runnerPID(r.path)
	deadline := time.Now().Add(waitLimit)
	for !procGone(rpid) {
		if time.Now().After(deadline) {
			t.Fatalf("the runner did not fail-stop: %s", p.stderr())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if status, outcome := r.e.jobOutcome(r.path, "j1"); outcome != "" || status == "failed" || status == "completed" || status == "quarantined" {
		t.Fatalf("the stopped runner wrote %s/%s", status, outcome)
	}
	pid := r.dispatchPID()
	old := r.token("j1")
	p2 := r.run(nil)
	p2.waitReady(t)
	r.waitAdopted("j1", old)
	release()
	if status, outcome := r.waitOutcome("j1"); status != "completed" || outcome != "ok" {
		t.Fatalf("job is %s/%s, want completed/ok", status, outcome)
	}
	// The first runner's strace ends once the worker did; it carries the
	// runner's exit code.
	if code := p.waitExit(t); code != 5 {
		t.Fatalf("first runner exit %d, want 5: %s", code, p.stderr())
	}
	var ws syscall.WaitStatus
	if _, err := syscall.Wait4(pid, &ws, 0, nil); err != nil && err != syscall.ECHILD {
		t.Fatalf("wait4 %d: %v", pid, err)
	}
	if n := r.execs(); n != 1 {
		t.Fatalf("execve of the executor = %d, want 1", n)
	}
	r.noSecondAttempt(1)
	r.stop(p2)
}

// P1: the recorded PID was reused by an unrelated process before the restart.
// Process IDs can only be placed by writing ns_last_pid, so the scenario runs
// in a user and PID namespace: this test starts itself inside
// `unshare -Urpm --fork --mount-proc` with IC_PIDREUSE_INNER set. Expected: the
// start time differs, so the recorded process is dead, not adopted, the job is
// indeterminate and the unrelated process is not signalled.
func TestCLIRecoveryPIDReuse(t *testing.T) {
	if os.Getenv("IC_PIDREUSE_INNER") == "" {
		if out, err := exec.Command("unshare", "-Urpm", "--fork", "--mount-proc", "true").CombinedOutput(); err != nil {
			t.Skipf("P1 needs unshare -Urpm --fork --mount-proc: %v: %s", err, strings.TrimSpace(string(out)))
		}
		cmd := exec.Command("unshare", "-Urpm", "--fork", "--mount-proc", os.Args[0], "-test.run=^TestCLIRecoveryPIDReuse$", "-test.v", "-test.timeout=10m")
		cmd.Env = append(os.Environ(), "IC_PIDREUSE_INNER=1")
		out, err := cmd.CombinedOutput()
		text := string(out)
		if i := strings.Index(text, "--- SKIP"); i >= 0 {
			t.Skipf("P1 skipped inside the namespace: %s", firstLine(text[i:]))
		}
		if err != nil {
			t.Fatalf("P1 inside the namespace failed: %v\n%s", err, tail(text, 4000))
		}
		if !strings.Contains(text, "--- PASS: TestCLIRecoveryPIDReuse") {
			t.Fatalf("P1 did not report a pass:\n%s", tail(text, 2000))
		}
		return
	}
	r := newRec(t)
	r.gateJob("j1", nil)
	r.crashAt("after_identity_before_running_cas", "")
	pid := r.dispatchPID()
	if pid <= 1 || procGone(pid) {
		t.Fatalf("recorded pid %d is not a live worker", pid)
	}
	old := r.token("j1")
	r.emptyAttemptCgroup("j1")
	waitProcGone(t, pid, "the worker")
	// This test is PID 1 of the namespace, so every orphan of the killed runner
	// is its child. A PID stays allocated while any process still has it as its
	// process group, so every zombie of the worker's group is reaped, not only
	// the worker.
	reapAll(t, pid)
	// The Go runtime may start a thread (which takes a PID) between the write
	// and the fork, so the target of the write adapts to the PID actually given.
	var other *exec.Cmd
	next := pid - 1
	for i := 0; i < 100 && other == nil; i++ {
		if err := os.WriteFile("/proc/sys/kernel/ns_last_pid", []byte(strconv.Itoa(next)), 0o644); err != nil {
			t.Skipf("cannot write ns_last_pid: %v", err)
		}
		c := exec.Command("sleep", "300")
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		got := c.Process.Pid
		if i < 6 {
			t.Logf("ns_last_pid=%d gave pid %d, want %d", next, got, pid)
		}
		if got == pid {
			other = c
			break
		}
		_ = c.Process.Kill()
		_ = c.Wait()
		if next += pid - got; next < 1 {
			next = 1
		}
	}
	if other == nil {
		t.Skipf("could not make a process take the recorded pid %d", pid)
	}
	t.Cleanup(func() { _ = other.Process.Kill(); _ = other.Wait() })

	p := r.run(nil)
	p.waitReady(t)
	status, outcome := r.waitOutcome("j1")
	if status != "failed" || outcome != "indeterminate" {
		t.Fatalf("job is %s/%s, want failed/indeterminate", status, outcome)
	}
	if !strings.Contains(p.stderr(), "recorded process: dead") {
		t.Fatalf("the reused pid was not judged dead: %s", p.stderr())
	}
	if got := r.e.count(r.path, `SELECT COUNT(*) FROM scheduler_job_attempts WHERE job_id = 'j1' AND claim_token != '`+old+`'`); got != 0 {
		t.Fatal("the unrelated process was adopted: the attempt carries a new token")
	}
	if s := procState(pid); s == 0 || s == 'Z' || s == 'X' {
		t.Fatalf("the unrelated process %d was signalled (state %q)", pid, string(s))
	}
	if n := r.execs(); n != 1 {
		t.Fatalf("execve of the executor = %d, want 1", n)
	}
	r.stop(p)
}

// reapAll waits for every exited child of this process until pid is gone from
// /proc and no zombie is left.
func reapAll(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var ws syscall.WaitStatus
		for {
			if w, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil); err != nil || w <= 0 {
				break
			}
		}
		if _, err := os.Stat("/proc/" + strconv.Itoa(pid)); err != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pid %d is still in /proc after the orphans were reaped", pid)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
