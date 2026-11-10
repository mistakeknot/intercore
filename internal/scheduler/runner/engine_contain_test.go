//go:build icsched_test && linux

package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/mistakeknot/intercore/internal/scheduler"
)

// The containment tests need a cgroup v2 subtree this user may write to (a
// systemd user scope started with Delegate=yes, or a user slice). Without one
// they SKIP and say why: a skipped containment test is not a pass.

// containRig is a rig whose runner is contained in a private subtree below the
// test process's own cgroup. At teardown no attempt cgroup may hold a process.
func containRig(t *testing.T) (*rig, Options) {
	t.Helper()
	own, err := ownCgroupDir()
	if err != nil {
		t.Skipf("containment tests need cgroup v2: %v", err)
	}
	root := filepath.Join(own, fmt.Sprintf("ictest-%d-%d", os.Getpid(), time.Now().UnixNano()))
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Skipf("containment tests need a delegated cgroup subtree (cannot create %s): %v", root, err)
	}
	probe, err := newCgroupMgr(Options{CgroupDir: root, ExtinctionBound: time.Second})
	if err != nil {
		_ = os.Remove(root)
		t.Skipf("containment tests need a delegated cgroup subtree: %v", err)
	}
	probe.release()

	r := newRig(t)
	t.Cleanup(func() { teardownSubtree(t, root) })
	o := r.opts()
	lg := &logBuf{}
	o.Stderr = lg
	t.Cleanup(func() {
		if t.Failed() || testing.Verbose() {
			t.Logf("runner log:\n%s", lg.String())
		}
	})
	o.NoContainment = false
	o.CgroupDir = root
	o.ExtinctionBound = 5 * time.Second
	return r, o
}

// teardownSubtree is the no-survivor check: after the test every attempt cgroup
// must be empty. A populated one is reported, then killed so it cannot leak.
func teardownSubtree(t *testing.T, root string) {
	ents, _ := os.ReadDir(root)
	for _, e := range ents {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "attempt-") {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if pop, err := populated(dir); err == nil && pop {
			t.Errorf("survivor: %s still has processes after the test", dir)
		}
		_ = os.WriteFile(filepath.Join(dir, "cgroup.kill"), []byte("1"), 0o644)
		for i := 0; i < 200; i++ {
			if pop, _ := populated(dir); !pop {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		_ = os.Remove(dir)
	}
	_ = os.Remove(filepath.Join(root, "runner"))
	_ = os.Remove(root)
}

// logBuf collects the runner's stderr and prints it only when a test fails.
type logBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func (r *rig) attemptPaths(id string) (containment, cgroup string) {
	r.t.Helper()
	var cg *string
	if err := r.raw.QueryRow(`SELECT containment, cgroup_path FROM scheduler_job_attempts WHERE job_id = ? ORDER BY attempt_no DESC LIMIT 1`, id).Scan(&containment, &cg); err != nil {
		r.t.Fatal(err)
	}
	if cg != nil {
		cgroup = *cg
	}
	return
}

// outputOf is the -o base the runner gave the executor for the job's last attempt.
func (r *rig) outputOf(id string) string {
	r.t.Helper()
	var o *string
	if err := r.raw.QueryRow(`SELECT output_file FROM scheduler_job_attempts WHERE job_id = ? ORDER BY attempt_no DESC LIMIT 1`, id).Scan(&o); err != nil || o == nil {
		r.t.Fatalf("no output_file for %s: %v", id, err)
	}
	return *o
}

func (r *rig) jobOfToken(token string) string {
	a, err := r.store.AttemptByToken(context.Background(), token)
	if err != nil {
		return ""
	}
	return a.JobID
}

// pidOf waits for the dispatch row of a job to carry a pid.
func (r *rig) pidOf(id string) int {
	r.t.Helper()
	var pid int
	until(r.t, "a pid for "+id, func() bool {
		d := r.dispatchOf(id)
		if d == "" {
			return false
		}
		_ = r.raw.QueryRow(`SELECT COALESCE(pid,0) FROM dispatches WHERE id = ?`, d).Scan(&pid)
		return pid > 1
	})
	return pid
}

// gone reports that no process has the pid (a zombie counts as gone: the
// cgroup, not the pid table, is what the runner proves empty).
func gone(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	i := strings.LastIndexByte(string(b), ')')
	return i >= 0 && i+2 < len(b) && b[i+2] == 'Z'
}

// linkInvariant: every attempt that started has a dispatch linked, and no
// dispatch is linked to two attempts.
func (r *rig) linkInvariant() {
	r.t.Helper()
	if n := r.count(`SELECT COUNT(*) FROM scheduler_job_attempts WHERE launch_result = 'started' AND dispatch_id IS NULL`); n != 0 {
		r.t.Errorf("%d started attempt(s) have no dispatch link", n)
	}
	if n := r.count(`SELECT COUNT(*) FROM (SELECT dispatch_id FROM scheduler_job_attempts WHERE dispatch_id IS NOT NULL GROUP BY dispatch_id HAVING COUNT(*) > 1)`); n != 0 {
		r.t.Errorf("%d dispatch(es) linked to more than one attempt", n)
	}
}

func (r *rig) cancel(id string) {
	r.t.Helper()
	if _, err := r.store.CancelJob(context.Background(), id, time.Now()); err != nil {
		r.t.Fatalf("cancel %s: %v", id, err)
	}
}

// aliveRunner fails the test if the runner has returned.
func aliveRunner(t *testing.T, run *running) {
	t.Helper()
	select {
	case <-run.done:
		t.Fatalf("the runner exited: %v", run.err)
	default:
	}
}

// Each attempt runs in its own cgroup, the executor is inside it, and the
// empty directory is removed after the finalize commits.
func TestAttemptRunsInItsOwnCgroup(t *testing.T) {
	r, o := containRig(t)
	gate := r.gateJob("g")
	run := start(t, o)
	r.waitStatus("g", scheduler.StatusRunning)

	containment, cg := r.attemptPaths("g")
	if containment != "cgroup" || cg == "" {
		t.Fatalf("containment=%q cgroup_path=%q", containment, cg)
	}
	if filepath.Dir(cg) != o.CgroupDir || !strings.HasPrefix(filepath.Base(cg), "attempt-q") || !strings.HasSuffix(cg, "-g-1") {
		t.Fatalf("cgroup_path %s is not <subtree>/attempt-<queue>-g-1", cg)
	}
	pid := r.pidOf("g")
	procs, err := os.ReadFile(filepath.Join(cg, "cgroup.procs"))
	if err != nil || !strings.Contains("\n"+string(procs), "\n"+strconv.Itoa(pid)+"\n") {
		t.Fatalf("executor pid %d is not in %s (%q, %v)", pid, cg, procs, err)
	}
	if pop, _ := populated(cg); !pop {
		t.Fatal("the attempt cgroup is not populated while the job runs")
	}

	release(t, gate)
	r.waitStatus("g", scheduler.StatusCompleted)
	run.stop(t)
	if _, err := os.Stat(cg); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the finished attempt's cgroup was not removed: %v", err)
	}
	r.linkInvariant()
}

// C4: cancel while running ends the process, the job is cancelled, and it is
// not retried.
func TestCancelWhileRunningInCgroup(t *testing.T) {
	r, o := containRig(t)
	r.job("h", "sleep 60", func(j *scheduler.SpawnJob) { j.MaxRetries = 3; j.Idempotent = true })
	run := start(t, o)
	r.waitStatus("h", scheduler.StatusRunning)
	pid := r.pidOf("h")
	_, cg := r.attemptPaths("h")
	r.cancel("h")
	r.waitStatus("h", scheduler.StatusCancelled)
	run.stop(t)
	if r.outcome("h") != scheduler.ClassCancelled || r.attempts("h") != 1 || r.runs() != 1 {
		t.Fatalf("outcome=%q attempts=%d runs=%d", r.outcome("h"), r.attempts("h"), r.runs())
	}
	until(t, "the executor to be gone", func() bool { return gone(pid) })
	if _, err := os.Stat(cg); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cgroup %s remains: %v", cg, err)
	}
	r.linkInvariant()
}

// cancelAt returns a barrier hook that cancels the job of the first token to
// reach point.
func cancelAt(r *rig, point string, hit *atomic.Bool) func(string, string) {
	return func(p, token string) {
		if p != point || !hit.CompareAndSwap(false, true) {
			return
		}
		r.cancel(r.jobOfToken(token))
	}
}

// The cancel races of the launch path (C1, C2, C3). Each ends cancelled, never
// leaves a process, and never takes the runner down: a control job still runs.
func TestCancelDuringLaunch(t *testing.T) {
	cases := []struct {
		name     string
		point    string
		mode     string
		maxRuns  int
		wantRuns int // -1: any up to maxRuns
	}{
		{"C1 after claim", "after_claim", "hang", 0, 0},
		{"C2 after link before start", "after_link_before_start", "hang", 1, -1},
		{"C3 after identity before running CAS", "after_identity_before_running_cas", "hang", 1, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, o := containRig(t)
			r.job("x", tc.mode, func(j *scheduler.SpawnJob) { j.MaxRetries = 3; j.Idempotent = true })
			var hit atomic.Bool
			o.hooks.barrier = cancelAt(r, tc.point, &hit)
			run := start(t, o)
			r.waitStatus("x", scheduler.StatusCancelled)
			if !hit.Load() {
				t.Fatal("the barrier never fired: the test proves nothing")
			}
			aliveRunner(t, run)
			if r.outcome("x") != scheduler.ClassCancelled {
				t.Fatalf("outcome %q", r.outcome("x"))
			}
			if r.attempts("x") != 1 {
				t.Fatalf("attempts = %d (a cancelled job is never retried)", r.attempts("x"))
			}
			if n := r.runs(); n > tc.maxRuns || (tc.wantRuns >= 0 && n != tc.wantRuns) {
				t.Fatalf("executor runs = %d, want %d", n, tc.maxRuns)
			}
			if tc.maxRuns == 0 && r.dispatches("x") != 0 {
				t.Fatalf("a dispatch was created for a job cancelled before launch")
			}

			// The runner keeps running: a control job still completes.
			r.job("ctrl", "ok")
			r.waitStatus("ctrl", scheduler.StatusCompleted)
			run.stop(t)
			if _, cg := r.attemptPaths("x"); cg != "" {
				if _, err := os.Stat(cg); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("cgroup %s remains: %v", cg, err)
				}
			}
			r.linkInvariant()
		})
	}
}

// E2c: a lost entry whose cgroup is still populated is re-inserted as
// completing; extinction kills the descendant and proves the cgroup empty;
// then evidence and finalize run, with one execve.
func TestRepairWithLiveDescendantRunsExtinction(t *testing.T) {
	r, o := containRig(t)
	r.job("x", "spawn-child")
	var cap flCapture
	var dropped atomic.Bool
	var wasPopulated atomic.Bool
	drop := cap.dropOnce(func(string) bool { return true }, &dropped)
	o.hooks.onInFlight = cap.hook
	o.hooks.afterExitBeforeEvidence = func(token string) {
		_, cg := r.attemptPaths("x")
		if pop, _ := populated(cg); pop {
			wasPopulated.Store(true)
		}
		drop(token)
	}
	run := start(t, o)
	r.waitStatus("x", scheduler.StatusCompleted)
	run.stop(t)
	if !dropped.Load() || !wasPopulated.Load() {
		t.Fatalf("dropped=%v populated-at-drop=%v: the test proves nothing", dropped.Load(), wasPopulated.Load())
	}
	if r.outcome("x") != scheduler.ClassOK || r.attempts("x") != 1 || r.runs() != 1 {
		t.Fatalf("outcome=%q attempts=%d runs=%d", r.outcome("x"), r.attempts("x"), r.runs())
	}
	out := r.outputOf("x")
	b, err := os.ReadFile(out + ".child")
	if err != nil {
		t.Fatalf("no child pid recorded: %v", err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if pid <= 1 || !gone(pid) {
		t.Fatalf("the descendant %d survived the extinction", pid)
	}
	r.linkInvariant()
}

// A descendant that left the session and cleared its environment is still in
// the attempt's cgroup, so extinction still ends it.
func TestExtinctionEndsDescendantThatEscapedSessionAndEnv(t *testing.T) {
	r, o := containRig(t)
	r.job("x", "setsid-clear-env")
	// Positive control, taken once the worker has exited and before extinction:
	// the descendant is alive, has a session of its own, no environment, and
	// sits in the attempt's cgroup. Without this the test could pass because
	// the recorded pid was never the long-lived descendant.
	var seen atomic.Value
	o.hooks.barrier = func(p, token string) {
		if p != "after_exit_before_evidence" || seen.Load() != nil {
			return
		}
		b, err := os.ReadFile(r.outputOf("x") + ".child")
		if err != nil {
			seen.Store("no child file: " + err.Error())
			return
		}
		pid := strings.TrimSpace(string(b))
		// $! names the env process that is about to exec setsid and then sleep;
		// wait until it has become the sleep, in its own session.
		var st []byte
		for i := 0; i < 500; i++ {
			st, err = os.ReadFile("/proc/" + pid + "/stat")
			if err != nil || strings.HasPrefix(string(st[strings.IndexByte(string(st), '(')+1:]), "sleep)") {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if err != nil {
			seen.Store("descendant " + pid + " is not alive before extinction: " + err.Error())
			return
		}
		f := strings.Fields(string(st[strings.LastIndexByte(string(st), ')')+2:]))
		env, _ := os.ReadFile("/proc/" + pid + "/environ")
		cg, _ := os.ReadFile("/proc/" + pid + "/cgroup")
		switch {
		case len(f) < 4 || f[3] != pid:
			seen.Store("descendant " + pid + " is not its own session leader: " + string(st))
		case len(env) != 0:
			seen.Store("descendant " + pid + " kept an environment")
		case !strings.Contains(string(cg), "/attempt-q") || !strings.HasSuffix(strings.TrimSpace(string(cg)), "-x-1"):
			seen.Store("descendant " + pid + " is not in the attempt cgroup: " + string(cg))
		default:
			seen.Store("ok")
		}
	}
	run := start(t, o)
	r.waitStatus("x", scheduler.StatusCompleted)
	run.stop(t)
	if v, _ := seen.Load().(string); v != "ok" {
		t.Fatalf("positive control failed: %q", v)
	}
	b, err := os.ReadFile(r.outputOf("x") + ".child")
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if pid <= 1 || !gone(pid) {
		t.Fatalf("the escaped descendant %d survived", pid)
	}
	if r.outcome("x") != scheduler.ClassOK {
		t.Fatalf("outcome %q", r.outcome("x"))
	}
}

// E2d (launched half): a healthy long job spans at least four repair passes and
// is untouched: no stage change, no kill, no second attempt. The adopted half
// needs the restart recovery of slice 4.
func TestHealthyJobSurvivesRepairPasses(t *testing.T) {
	r, o := containRig(t)
	r.job("s", "sleep 120")
	var passes atomic.Int64
	o.hooks.repairPass = func() { passes.Add(1) }
	run := start(t, o)
	r.waitStatus("s", scheduler.StatusRunning)
	pid := r.pidOf("s")
	base := passes.Load()
	until(t, "four repair passes", func() bool { return passes.Load() >= base+4 })
	if st := r.get("s").Status; st != scheduler.StatusRunning {
		t.Fatalf("job is %s after repair passes", st)
	}
	if gone(pid) {
		t.Fatal("repair killed the healthy process")
	}
	if r.attempts("s") != 1 || r.dispatches("s") != 1 || r.runs() != 1 {
		t.Fatalf("attempts=%d dispatches=%d runs=%d", r.attempts("s"), r.dispatches("s"), r.runs())
	}
	r.cancel("s")
	r.waitStatus("s", scheduler.StatusCancelled)
	run.stop(t)
}

// P7: a fork storm while the cgroup is killed reaches populated 0 within the
// bound, and no process escapes.
func TestForkStormIsExtinguished(t *testing.T) {
	r, o := containRig(t)
	r.job("f", "fork-storm")
	run := start(t, o)
	r.waitStatus("f", scheduler.StatusRunning)
	until(t, "the storm to start", func() bool {
		_, err := os.Stat(r.outputOf("f") + ".started")
		return err == nil
	})
	_, cg := r.attemptPaths("f")
	r.cancel("f")
	r.waitStatus("f", scheduler.StatusCancelled)
	run.stop(t)
	if _, err := os.Stat(cg); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the storm's cgroup %s was not emptied and removed: %v", cg, err)
	}
	if r.outcome("f") != scheduler.ClassCancelled {
		t.Fatalf("outcome %q", r.outcome("f"))
	}
}

// P7 (failure half): when populated 0 is not observed within the bound the job
// is not terminalized and not retried: retry_blocked names the dispatch, the
// entry stays in flight, and the extinction is retried until it is proven.
func TestExtinctionTimeoutBlocksRetryAndDoesNotTerminalize(t *testing.T) {
	r, o := containRig(t)
	o.ExtinctionBound = 300 * time.Millisecond
	// The first extinctions are held back past the bound; the later ones are not.
	var calls atomic.Int64
	o.hooks.populatedDelay = func() time.Duration {
		if calls.Add(1) <= 2 {
			return time.Hour
		}
		return 0
	}
	// A failing job that its policy would retry at once: only retry_blocked
	// can keep it from running again.
	r.job("x", "receipt agent_failed", func(j *scheduler.SpawnJob) {
		j.Idempotent = true
		j.RetryPolicy = `{"max_attempts":5,"backoff":{"base_s":1,"factor":1,"max_s":1,"jitter":0},"retry_on":["agent_failed"]}`
	})
	run := start(t, o)

	until(t, "retry_blocked to be set", func() bool {
		var rb *string
		_ = r.raw.QueryRow(`SELECT retry_blocked FROM scheduler_jobs WHERE id = 'x'`).Scan(&rb)
		return rb != nil && strings.HasPrefix(*rb, "survivor_unknown:")
	})
	if j := r.get("x"); j.IsTerminal() {
		t.Fatalf("a job whose cgroup was not proven empty became %s", j.Status)
	}
	if r.attempts("x") != 1 {
		t.Fatalf("attempts = %d: the job was retried before the cgroup was proven empty", r.attempts("x"))
	}
	// Once extinction is proven the retry is held, not run: the job returns to
	// pending with the marker kept, and stays at one attempt and one execution.
	until(t, "the job to return to pending after extinction is proven", func() bool {
		j := r.get("x")
		if j.IsTerminal() {
			t.Fatalf("job ended %s/%s instead of returning to pending", j.Status, r.outcome("x"))
		}
		return j.Status == scheduler.StatusPending
	})
	time.Sleep(1500 * time.Millisecond) // past the 1 s backoff: a retry the marker failed to hold would be claimed by now
	run.stop(t)
	if r.attempts("x") != 1 || r.runs() != 1 {
		t.Fatalf("attempts=%d runs=%d: a blocked job was retried", r.attempts("x"), r.runs())
	}
	var rb *string
	_ = r.raw.QueryRow(`SELECT retry_blocked FROM scheduler_jobs WHERE id = 'x'`).Scan(&rb)
	if rb == nil || !strings.HasPrefix(*rb, "survivor_unknown:") {
		t.Fatalf("the retry block was lost: %v", rb)
	}
}

// Two job IDs that sanitize to the same text must not share a cgroup: one
// job's extinction would kill the other job's live worker.
func TestAttemptDirNamesAreInjective(t *testing.T) {
	ids := []string{"a/b", "a_b", "a b", "a:b", "ab", "a-b", "a~b", strings.Repeat("x", 80), strings.Repeat("x", 81), "é", "_"}
	// One runner uses one namespace; "" is a bare manager, and the two queue
	// namespaces share a subtree, so no name may repeat across them.
	for _, nss := range [][]string{{""}, {"q00000001", "q00000002"}} {
		seen := map[string]string{}
		for _, ns := range nss {
			for _, id := range ids {
				for _, n := range []int{1, 2, 11} {
					name := attemptDirName(ns, id, n)
					if len(name) > 200 || strings.ContainsAny(name, "/ \x00") {
						t.Fatalf("%q attempt %d: unusable cgroup name %q", id, n, name)
					}
					key := ns + "/" + id + "#" + strconv.Itoa(n)
					if prev, dup := seen[name]; dup {
						t.Fatalf("%s and %s share the cgroup name %q", prev, key, name)
					}
					seen[name] = key
				}
			}
		}
	}
	if got := attemptDirName("", "g", 1); got != "attempt-g-1" {
		t.Fatalf("a safe job ID keeps its readable name, got %q", got)
	}
}

// An attempt cgroup that still holds a process is never reused, an empty
// leftover is.
func TestCreateRefusesBusyCgroupReusesEmpty(t *testing.T) {
	_, o := containRig(t)
	m := &cgroupMgr{root: o.CgroupDir}
	p, f, err := m.create("reuse", 1)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	p2, f2, err := m.create("reuse", 1)
	if err != nil {
		t.Fatalf("an empty leftover directory must be reusable: %v", err)
	}
	f2.Close()
	if p2 != p {
		t.Fatalf("paths differ: %s %s", p, p2)
	}
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true}
	fd, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer fd.Close()
	cmd.SysProcAttr.CgroupFD = int(fd.Fd())
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start a process into %s: %v", p, err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait(); _ = os.Remove(p) }()
	if _, _, err := m.create("reuse", 1); !errors.Is(err, errCgroupBusy) {
		t.Fatalf("a populated attempt cgroup was reused: %v", err)
	}
}

// Two queues that share a delegated subtree never name the same attempt
// directory, even for the same job ID and attempt number.
func TestQueuesSharingASubtreeGetDistinctAttemptCgroups(t *testing.T) {
	_, o := containRig(t)
	a := &cgroupMgr{root: o.CgroupDir}
	b := &cgroupMgr{root: o.CgroupDir}
	a.setQueue("queue-A")
	b.setQueue("queue-B")
	pa, fa, err := a.create("same", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer fa.Close()
	defer os.Remove(pa)
	pb, fb, err := b.create("same", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer fb.Close()
	defer os.Remove(pb)
	if pa == pb {
		t.Fatalf("queues A and B share the attempt cgroup %s", pa)
	}
}

// An attempt cgroup that already holds a process when the attempt starts (a
// restored snapshot whose worker outlived the old runner) is recorded with its
// path, nothing is started, and the completion step kills the leftover and
// proves the cgroup empty before the job is finalized.
func TestBusyAttemptCgroupIsKilledAndProvenBeforeFinalize(t *testing.T) {
	r, o := containRig(t)
	row, err := r.store.ReadRunner(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m := &cgroupMgr{root: o.CgroupDir}
	m.setQueue(row.QueueID)
	p, f, err := m.create("b", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(p)
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(f.Fd())}
	if err := cmd.Start(); err != nil {
		f.Close()
		t.Skipf("cannot start a process into %s: %v", p, err)
	}
	f.Close()
	gone := make(chan struct{})
	go func() { _ = cmd.Wait(); close(gone) }()
	defer func() { _ = cmd.Process.Kill() }()

	r.job("b", "ok")
	run := start(t, o)
	until(t, "the job to become terminal", func() bool { return r.get("b").IsTerminal() })
	run.stop(t)

	select {
	case <-gone:
	default:
		t.Fatal("the leftover process of the busy attempt cgroup is still alive")
	}
	if r.runs() != 0 {
		t.Fatalf("the executor ran %d times although the attempt cgroup was busy", r.runs())
	}
	containment, cg := r.attemptPaths("b")
	if containment != "cgroup" || cg != p {
		t.Fatalf("containment=%q cgroup_path=%q, want cgroup %q", containment, cg, p)
	}
	if got := r.outcome("b"); got != "prestart" {
		t.Fatalf("outcome = %q, want prestart", got)
	}
	if pop, err := populated(p); err != nil || pop {
		t.Fatalf("the busy cgroup was not proven empty (populated=%v err=%v)", pop, err)
	}
}
