//go:build icsched_test && linux

package main

// Prior-epoch recovery through the real icsched_test binary (plan section 9:
// E2b, E2e, E2f, E3 to E5, R1c, P3c, P4 and the cgroup-mode kill matrix). A
// runner is held at a barrier, killed there with SIGKILL, and a second runner
// recovers from the database alone. Every runner runs under strace, so the
// launch count is observed from outside the process under test, and every
// runner is contained in a delegated cgroup v2 subtree so that "nothing runs
// outside the attempt cgroup" is checked in /proc. A test skips, with the
// reason, when strace cannot trace or no delegated subtree can be made.
//
// Recorded-PID recovery (the adopt half of E2d and E2e, barriers 5 to 7) and the
// fallback-mode sweep belong to later slices; the one barrier 5 case here is P4,
// which the boot change shortcut finishes without a process lookup.

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// delegate makes a cgroup v2 subtree under the test's own cgroup and points the
// runner at it. The test skips when that is not possible here. The cleanup is
// the no-survivor check: any attempt cgroup that still holds a process is
// reported, then killed and removed.
func (e *env) delegate() string {
	e.t.Helper()
	raw, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		e.t.Skipf("cgroup v2 not readable: %v", err)
	}
	own := ""
	for _, line := range strings.Split(string(raw), "\n") {
		if rest, ok := strings.CutPrefix(line, "0::"); ok {
			own = filepath.Join("/sys/fs/cgroup", filepath.Clean("/"+rest))
		}
	}
	if own == "" {
		e.t.Skip("no cgroup v2 entry in /proc/self/cgroup")
	}
	root := filepath.Join(own, fmt.Sprintf("icrec-%d-%d", os.Getpid(), time.Now().UnixNano()))
	if err := os.Mkdir(root, 0o755); err != nil {
		e.t.Skipf("recovery tests need a delegated cgroup subtree (cannot create %s): %v", root, err)
	}
	if _, err := os.Stat(filepath.Join(root, "cgroup.kill")); err != nil {
		_ = os.Remove(root)
		e.t.Skipf("recovery tests need cgroup.kill under %s: %v", root, err)
	}
	e.cgroupDir = root
	e.t.Cleanup(func() {
		ents, _ := os.ReadDir(root)
		for _, d := range ents {
			if !d.IsDir() || !strings.HasPrefix(d.Name(), "attempt-") {
				continue
			}
			dir := filepath.Join(root, d.Name())
			if populatedDir(dir) {
				e.t.Errorf("survivor: %s still has processes after the test", dir)
			}
			killDir(dir)
		}
		_ = os.Remove(filepath.Join(root, "runner"))
		_ = os.Remove(root)
	})
	return root
}

func populatedDir(dir string) bool {
	b, err := os.ReadFile(filepath.Join(dir, "cgroup.events"))
	return err == nil && strings.Contains(string(b), "populated 1")
}

// killDir kills everything in a cgroup, waits until it is empty and removes it.
func killDir(dir string) {
	_ = os.WriteFile(filepath.Join(dir, "cgroup.kill"), []byte("1"), 0o644)
	for i := 0; i < 500 && populatedDir(dir); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	_ = os.Remove(dir)
}

// procGone reports that pid no longer runs: no /proc entry, or a zombie.
func procGone(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	s := string(b)
	i := strings.LastIndex(s, ") ")
	return i > 0 && len(s) > i+2 && s[i+2] == 'Z'
}

func waitProcGone(t *testing.T, pid int, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !procGone(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("%s (pid %d) is still running", what, pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

var execPID = regexp.MustCompile(`^(\d+) `)

// execPIDs returns the pids of the successful execves whose line has needle.
func execPIDs(t *testing.T, log, needle string) []int {
	t.Helper()
	var pids []int
	for _, l := range execLines(mustRead(t, log), needle) {
		if m := execPID.FindStringSubmatch(l); m != nil {
			n, _ := strconv.Atoi(m[1])
			pids = append(pids, n)
		}
	}
	return pids
}

// rec is one recovery scenario: a database, a delegated subtree, a fake
// executor and the strace logs of every runner started so far.
type rec struct {
	t      *testing.T
	e      *env
	strace string
	fake   string
	path   string
	root   string
	logs   []string
}

func newRec(t *testing.T) *rec {
	t.Helper()
	strace := needStrace(t)
	e := newEnv(t)
	root := e.delegate()
	return &rec{t: t, e: e, strace: strace, fake: e.fakeScript(), path: e.newDB("q.db"), root: root}
}

// run starts a runner with a fast repair pass. extraEnv adds faults and barriers.
func (r *rec) run(extraEnv []string, extra ...string) *proc {
	r.t.Helper()
	log := filepath.Join(r.e.cwd, fmt.Sprintf("exec.%d.log", len(r.logs)))
	r.logs = append(r.logs, log)
	args := append([]string{"--dispatch-sh=" + r.fake, "--repair-interval=100ms"}, extra...)
	return r.e.tracedRunner(r.strace, "q.db", log, extraEnv, args...)
}

func (r *rec) lastLog() string { return r.logs[len(r.logs)-1] }

// execs is the number of observed launches of the executor over every runner.
func (r *rec) execs() int { return executorExecs(r.t, r.fake, r.logs...) }

// heldAt starts a runner that stops at point, waits until it got there and
// returns it with its barrier. faults is an IC_SCHED_FAULT value or "".
func (r *rec) heldAt(point, faults string) (*proc, *barrier) {
	r.t.Helper()
	bar := newBarrier(r.t, r.t.TempDir())
	envv := bar.env(point)
	if faults != "" {
		envv = append(envv, "IC_SCHED_FAULT="+faults)
	}
	p := r.run(envv)
	p.waitReady(r.t)
	bar.wait(point)
	return p, bar
}

// killHard SIGKILLs the held runner like a power cut that leaves the executor
// and its descendants running. strace is not killed: it would take its tracees
// down with it (PTRACE_O_EXITKILL), so it stays until the recovery kills the
// attempt cgroup, and the cleanup ends it if a test fails before that.
func (r *rec) killHard(p *proc) {
	r.t.Helper()
	pid := r.e.signalRunner(r.path, syscall.SIGKILL)
	waitProcGone(r.t, pid, "the killed runner")
	r.t.Cleanup(func() {
		_ = p.cmd.Process.Kill()
		select {
		case <-p.done:
		case <-time.After(waitLimit):
		}
	})
}

// crashAt runs a runner to point and kills it there.
func (r *rec) crashAt(point, faults string) {
	r.t.Helper()
	p, _ := r.heldAt(point, faults)
	r.killHard(p)
}

// waitOutcome waits for the job to have an outcome class.
func (r *rec) waitOutcome(id string) (status, outcome string) {
	r.t.Helper()
	deadline := time.Now().Add(waitLimit)
	for {
		status, outcome = r.e.jobOutcome(r.path, id)
		if outcome != "" {
			return status, outcome
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("job %s has no outcome (status %s)", id, status)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (r *rec) str(q string) string {
	var s sql.NullString
	r.e.query(r.path, q, &s)
	return s.String
}

func (r *rec) crashes(id string) int {
	return r.e.count(r.path, `SELECT crash_requeues FROM scheduler_jobs WHERE id = '`+id+`'`)
}

func (r *rec) attemptDir(id string) string {
	return r.str(`SELECT cgroup_path FROM scheduler_job_attempts WHERE job_id = '` + id + `' ORDER BY attempt_no DESC LIMIT 1`)
}

// stop drains the last runner and expects a clean exit.
func (r *rec) stop(p *proc) {
	r.t.Helper()
	if code := r.e.stopRunner(p, r.path); code != 0 {
		r.t.Fatalf("runner exit %d: %s", code, p.stderr())
	}
}

// receipts lists the receipt files the executor wrote under the working directory.
func (r *rec) receipts() []string {
	var out []string
	_ = filepath.WalkDir(r.e.cwd, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".receipt.json") {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// R1c: a runner killed at barrier 1 (claimed, nothing linked) four times in a
// row. Each restart excuses the attempt and requeues it, counted in
// crash_requeues; the fourth crash abandons the job. The executor never runs.
func TestCLIRecoveryCrashCapAbandons(t *testing.T) {
	r := newRec(t)
	r.e.addJob(r.path, "j1", "ok")
	for n := 0; n <= 3; n++ {
		r.crashAt("after_claim", "")
		if got := r.crashes("j1"); got != n {
			t.Fatalf("after crash %d: crash_requeues = %d, want %d", n+1, got, n)
		}
	}
	p := r.run(nil)
	p.waitReady(t)
	status, outcome := r.waitOutcome("j1")
	if status != "failed" || outcome != "abandoned" {
		t.Fatalf("job is %s/%s, want failed/abandoned", status, outcome)
	}
	if got := r.crashes("j1"); got != 3 {
		t.Fatalf("crash_requeues = %d, want 3", got)
	}
	r.stop(p)
	if n := r.execs(); n != 0 {
		t.Fatalf("the executor ran %d times", n)
	}
}

// E2b: a current-epoch row with no in-flight entry and no marker (the launcher
// is held at barrier 1 and its entry dropped) stops the runner with exit 5. The
// next runner recovers the unlinked launching row as a crash before the link
// and the job runs once.
func TestCLIRecoveryLostEntryBeforeLinkExits5(t *testing.T) {
	r := newRec(t)
	r.e.addJob(r.path, "j1", "ok")
	bar := newBarrier(t, t.TempDir())
	p := r.run(append(bar.env("after_claim"), "IC_SCHED_FAULT=drop_entry:after_claim"))
	p.waitReady(t)
	bar.wait("after_claim")
	if code := p.waitExit(t); code != 5 {
		t.Fatalf("runner exit %d, want 5: %s", code, p.stderr())
	}
	if !strings.Contains(p.stderr(), "invariant violated") {
		t.Fatalf("the fail-stop does not say why: %s", p.stderr())
	}
	if status, outcome := r.e.jobOutcome(r.path, "j1"); outcome != "" || status == "failed" || status == "completed" {
		t.Fatalf("the stopped runner finalized the job: %s/%s", status, outcome)
	}

	p2 := r.run(nil)
	p2.waitReady(t)
	r.e.waitJob(r.path, "j1", "completed")
	if got := r.crashes("j1"); got != 1 {
		t.Fatalf("crash_requeues = %d, want 1", got)
	}
	r.stop(p2)
	if n := r.execs(); n != 1 {
		t.Fatalf("execve of the executor = %d, want 1", n)
	}
}

// E2f: the launcher is held at barrier 3 (linked, not started), its entry is
// dropped, and in every other iteration the barrier is released at once so the
// launch races the repair pass. The runner exits 5 either way and writes no
// finalize or quarantine. There are 0 or 1 launches, the row stays linked, and
// no process runs outside the attempt cgroup. Without the release (0 launches)
// the next runner sees a linked row with no PID and an empty cgroup and ends it
// indeterminate; a process that did start is left to the recorded-PID recovery
// of a later slice, so only the invariants are checked for it.
func TestCLIRecoveryLostEntryWhileLinkedExits5(t *testing.T) {
	for i := 0; i < 20; i++ {
		release := i%2 == 0
		t.Run(fmt.Sprintf("i%02d_release_%v", i, release), func(t *testing.T) {
			r := newRec(t)
			r.e.addJob(r.path, "j1", "sleep 5")
			p, bar := r.heldAt("after_link_before_start", "drop_entry:after_link_before_start")
			if release {
				// The repair pass may stop the runner before this release
				// lands; either order must end in exit 5.
				bar.tryRelease(2 * time.Second)
			}
			if code := p.waitExit(t); code != 5 {
				t.Fatalf("runner exit %d, want 5: %s", code, p.stderr())
			}
			n := r.execs()
			if n > 1 || (!release && n != 0) {
				t.Fatalf("launches = %d (release=%v)", n, release)
			}
			status, outcome := r.e.jobOutcome(r.path, "j1")
			if outcome != "" || status == "failed" || status == "completed" || status == "quarantined" {
				t.Fatalf("the stopped runner wrote %s/%s", status, outcome)
			}
			if r.e.count(r.path, `SELECT COUNT(*) FROM scheduler_job_attempts WHERE job_id = 'j1' AND linked_at IS NOT NULL AND ended_at IS NULL`) != 1 {
				t.Fatalf("the attempt is not linked and open")
			}
			dir := r.attemptDir("j1")
			for _, pid := range execPIDs(t, r.lastLog(), bashOf(r.fake)) {
				cg, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cgroup")
				if err == nil && !strings.HasSuffix(strings.TrimSpace(string(cg)), "/"+filepath.Base(dir)) {
					t.Fatalf("executor %d runs outside its attempt cgroup: %s", pid, cg)
				}
			}
			if release {
				return // the teardown kills what started
			}
			p2 := r.run(nil)
			p2.waitReady(t)
			if status, outcome := r.waitOutcome("j1"); outcome != "indeterminate" || status != "failed" {
				t.Fatalf("job is %s/%s, want failed/indeterminate", status, outcome)
			}
			r.stop(p2)
			if n := r.execs(); n != 0 {
				t.Fatalf("launches = %d, want 0", n)
			}
		})
	}
}

// E2e (4b half): the entry is dropped while the executor still runs. The runner
// exits 5 and touches neither the database nor the cgroup: the executor keeps
// running in its attempt cgroup, is reparented to the test's subreaper and is
// reaped there, leaving no zombie. Adopting it is a later slice.
func TestCLIRecoveryLostEntryWhileRunningExits5(t *testing.T) {
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		t.Skipf("cannot become a subreaper: %v", err)
	}
	t.Cleanup(func() { _ = unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 0, 0, 0, 0) })
	r := newRec(t)
	r.e.addJob(r.path, "j1", "sleep 4")
	p, bar := r.heldAt("after_identity_before_running_cas", "drop_entry:after_identity_before_running_cas")
	_ = bar
	rpid := r.e.runnerPID(r.path)
	// The runner is blocked at the barrier; the repair pass is what stops it.
	deadline := time.Now().Add(waitLimit)
	for !procGone(rpid) {
		if time.Now().After(deadline) {
			t.Fatalf("the runner did not fail-stop: %s", p.stderr())
		}
		time.Sleep(5 * time.Millisecond)
	}
	dir := r.attemptDir("j1")
	if !populatedDir(dir) {
		t.Fatalf("the executor's attempt cgroup %s was emptied by the stopped runner", dir)
	}
	if status, outcome := r.e.jobOutcome(r.path, "j1"); outcome != "" || status == "failed" || status == "completed" || status == "quarantined" {
		t.Fatalf("the stopped runner wrote %s/%s", status, outcome)
	}
	if code := p.waitExit(t); code != 5 {
		t.Fatalf("runner exit %d, want 5: %s", code, p.stderr())
	}
	pids := execPIDs(t, r.lastLog(), bashOf(r.fake))
	if len(pids) != 1 {
		t.Fatalf("launches = %d, want 1", len(pids))
	}
	var ws syscall.WaitStatus
	if _, err := syscall.Wait4(pids[0], &ws, 0, nil); err != nil {
		t.Fatalf("the executor was not reparented to the subreaper: %v", err)
	}
	if _, err := os.Stat("/proc/" + strconv.Itoa(pids[0])); err == nil {
		t.Fatalf("executor %d is still in /proc after it was reaped", pids[0])
	}
}

// receiptScenario runs a "receipt quota_exhausted" job to barrier 8 (collected,
// not finalized), kills the runner, lets edit change the receipt and restarts.
func receiptScenario(t *testing.T, edit func(r *rec, path string)) *rec {
	t.Helper()
	r := newRec(t)
	r.e.addJob(r.path, "j1", "receipt quota_exhausted")
	r.crashAt("after_collect_before_finalize", "")
	if got := r.str(`SELECT evidence_class FROM scheduler_job_attempts WHERE job_id = 'j1'`); got != "quota_exhausted" {
		t.Fatalf("evidence_class at the barrier = %q", got)
	}
	files := r.receipts()
	if len(files) != 1 {
		t.Fatalf("receipts = %v", files)
	}
	edit(r, files[0])
	p := r.run(nil)
	p.waitReady(t)
	t.Cleanup(func() {})
	status, outcome := r.waitOutcome("j1")
	if outcome != "capacity" {
		t.Fatalf("job is %s/%s, want outcome capacity from the stored evidence_class", status, outcome)
	}
	r.stop(p)
	if n := r.execs(); n != 1 {
		t.Fatalf("execve of the executor = %d, want 1", n)
	}
	if got := r.e.count(r.path, `SELECT COUNT(*) FROM scheduler_job_attempts WHERE job_id = 'j1' AND outcome_class = 'capacity'`); got < 1 {
		t.Fatalf("no attempt carries the capacity outcome")
	}
	return r
}

// E3: the receipt is deleted between the kill and the restart. The class comes
// from the stored evidence_class.
func TestCLIRecoveryReceiptDeletedAfterEvidence(t *testing.T) {
	receiptScenario(t, func(r *rec, path string) {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	})
}

// E4: the receipt is rewritten to another class. The class is unchanged: the
// file is not read again once the evidence is stored.
func TestCLIRecoveryReceiptRewrittenAfterEvidence(t *testing.T) {
	receiptScenario(t, func(r *rec, path string) {
		id := r.str(`SELECT dispatch_id FROM scheduler_job_attempts WHERE job_id = 'j1'`)
		body := fmt.Sprintf(`{"attempt_id":%q,"failure_class":"agent_failed"}`+"\n", id)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	})
}

// E5: a receipt that names another dispatch is not evidence. The runner is held
// after the executor exits, the receipt is rewritten to a foreign id, and the
// stored evidence is class none with source unbound_receipt.
func TestCLIRecoveryUnboundReceiptIsRejected(t *testing.T) {
	r := newRec(t)
	r.e.addJob(r.path, "j1", "receipt quota_exhausted")
	p, bar := r.heldAt("after_exit_before_evidence", "")
	files := r.receipts()
	if len(files) != 1 {
		t.Fatalf("receipts = %v", files)
	}
	body := `{"attempt_id":"disp-of-another-attempt","failure_class":"quota_exhausted"}` + "\n"
	if err := os.WriteFile(files[0], []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	bar.release()
	status, outcome := r.waitOutcome("j1")
	if outcome != "failed_unclassified" {
		t.Fatalf("job is %s/%s, want outcome failed_unclassified", status, outcome)
	}
	if c, s := r.str(`SELECT evidence_class FROM scheduler_job_attempts WHERE job_id = 'j1'`),
		r.str(`SELECT evidence_source FROM scheduler_job_attempts WHERE job_id = 'j1'`); c != "none" || s != "unbound_receipt" {
		t.Fatalf("evidence class %q source %q, want none/unbound_receipt", c, s)
	}
	r.stop(p)
}

// P4: a runner killed after the PID was recorded (barrier 5), then a restart on
// a different boot_id: the boot change shortcut terminalizes the dispatch as
// reconcile/reboot (not as a survivor) and finishes the job indeterminate
// without the recorded-PID check. The faked boot change leaves the attempt
// cgroup in place, so the pipeline's extinction step still empties it (after a
// real reboot the directory is gone and that step is a no-op); what the test
// cannot observe is the absence of a /proc lookup, only that the row took the
// reboot path: error message ending in reboot, no orphans counted, retry not blocked.
func TestCLIRecoveryBootChangeShortcut(t *testing.T) {
	r := newRec(t)
	r.e.addJob(r.path, "j1", "sleep 20")
	r.crashAt("after_identity_before_running_cas", "boot_id:boot-A")
	pids := execPIDs(t, r.lastLog(), bashOf(r.fake))
	if len(pids) != 1 {
		t.Fatalf("launches = %d, want 1", len(pids))
	}
	if r.e.count(r.path, `SELECT COUNT(*) FROM dispatches WHERE pid IS NOT NULL`) != 1 {
		t.Fatal("the PID was not recorded at barrier 5: this is not a recorded-PID row")
	}
	p := r.run([]string{"IC_SCHED_FAULT=boot_id:boot-B"})
	p.waitReady(t)
	status, outcome := r.waitOutcome("j1")
	if status != "failed" || outcome != "indeterminate" {
		t.Fatalf("job is %s/%s, want failed/indeterminate", status, outcome)
	}
	if got := r.str(`SELECT error_message FROM dispatches LIMIT 1`); !strings.HasSuffix(got, ": reboot") {
		t.Fatalf("dispatch error_message = %q, want a reboot reconcile", got)
	}
	if got := r.str(`SELECT terminal_source FROM dispatch_terminals LIMIT 1`); got != "reconcile" {
		t.Fatalf("terminal source = %q, want reconcile", got)
	}
	if got := r.e.count(r.path, `SELECT COALESCE(SUM(orphans_killed), 0) FROM dispatch_terminals`); got != 0 {
		t.Fatalf("orphans_killed = %d: the row took the survivor path", got)
	}
	waitProcGone(t, pids[0], "the executor (its attempt cgroup was emptied)")
	r.stop(p)
	if n := r.execs(); n != 1 {
		t.Fatalf("execve of the executor = %d, want 1", n)
	}
}

// The cgroup-mode kill matrix: a runner killed at each barrier a recovery in
// this slice handles, then restarted. Each case names the expected end state;
// "execs" is the launches over both runners and every case has one attempt and
// (when a dispatch exists) one terminal row.
func TestCLIRecoveryKillMatrix(t *testing.T) {
	type tc struct {
		name    string
		point   string
		mode    string
		faults  string
		prepare func(r *rec) // between the kill and the restart
		status  string
		outcome string
		attempt int // attempts at the end
		crashes int
		execs   int
		check   func(r *rec)
	}
	cases := []tc{
		{name: "b1_after_claim", point: "after_claim", mode: "ok", status: "completed", outcome: "ok", attempt: 2, crashes: 1, execs: 1},
		{name: "b2_in_link_before_commit", point: "in_link_before_commit", mode: "ok", status: "completed", outcome: "ok", attempt: 2, crashes: 1, execs: 1},
		{name: "b3_after_link_before_start", point: "after_link_before_start", mode: "ok", status: "failed", outcome: "indeterminate", attempt: 1, execs: 0,
			check: func(r *rec) {
				if got := r.e.count(r.path, `SELECT COUNT(*) FROM dispatch_terminals`); got != 1 {
					r.t.Fatalf("terminal rows = %d, want 1", got)
				}
				if dir := r.attemptDir("j1"); dir != "" {
					if _, err := os.Stat(dir); err == nil {
						r.t.Fatalf("the proven-empty attempt cgroup %s was not removed", dir)
					}
				}
			}},
		// P3c: the executor is alive in the attempt cgroup, no PID recorded.
		{name: "b4_alive_sleep", point: "after_start_before_identity", mode: "sleep 20", status: "failed", outcome: "indeterminate", attempt: 1, execs: 1,
			check: func(r *rec) {
				pid := execPIDs(r.t, r.logs[0], bashOf(r.fake))[0]
				waitProcGone(r.t, pid, "the executor")
				if got := r.e.count(r.path, `SELECT COALESCE(SUM(orphans_killed), 0) FROM dispatch_terminals`); got < 1 {
					r.t.Fatalf("orphans_killed = %d, want at least 1", got)
				}
				if got := r.str(`SELECT retry_blocked FROM scheduler_jobs WHERE id = 'j1'`); got != "" {
					r.t.Fatalf("retry_blocked = %q, want none: the cgroup is the proof", got)
				}
				if dir := r.attemptDir("j1"); populatedDir(dir) {
					r.t.Fatalf("attempt cgroup %s is not empty", dir)
				}
			}},
		{name: "b4_alive_descendant", point: "after_start_before_identity", mode: "spawn-child", status: "failed", outcome: "indeterminate", attempt: 1, execs: 1,
			// The descendant must exist and be alive before the restart, or
			// the extinction assertion below would pass without checking it.
			prepare: func(r *rec) {
				for i := 0; i < 500 && r.liveChild() == 0; i++ {
					time.Sleep(10 * time.Millisecond)
				}
				if r.liveChild() == 0 {
					r.t.Fatalf("the executor never started a live descendant")
				}
			},
			check: func(r *rec) {
				child := r.recordedChild()
				if child <= 0 {
					r.t.Fatalf("no descendant pid recorded")
				}
				waitProcGone(r.t, child, "the executor's descendant")
			}},
		{name: "b4_dead", point: "after_start_before_identity", mode: "sleep 20", status: "failed", outcome: "indeterminate", attempt: 1, execs: 1,
			prepare: func(r *rec) {
				dir := r.attemptDir("j1")
				_ = os.WriteFile(filepath.Join(dir, "cgroup.kill"), []byte("1"), 0o644)
				for i := 0; i < 500 && populatedDir(dir); i++ {
					time.Sleep(10 * time.Millisecond)
				}
				if populatedDir(dir) {
					r.t.Fatalf("cannot empty %s", dir)
				}
			},
			check: func(r *rec) {
				if got := r.e.count(r.path, `SELECT COALESCE(SUM(orphans_killed), 0) FROM dispatch_terminals`); got != 0 {
					r.t.Fatalf("orphans_killed = %d, want 0 for an empty cgroup", got)
				}
			}},
		{name: "b8_after_collect_before_finalize", point: "after_collect_before_finalize", mode: "ok", status: "completed", outcome: "ok", attempt: 1, execs: 1},
		{name: "b9_in_finalize_before_commit", point: "in_finalize_before_commit", mode: "ok", status: "completed", outcome: "ok", attempt: 1, execs: 1},
		{name: "b10_after_finalize_commit", point: "after_finalize_commit", mode: "ok", status: "completed", outcome: "ok", attempt: 1, execs: 1},
		{name: "b11_spawn_error", point: "after_launch_result_commit", mode: "ok", faults: "eagain_start:1", status: "failed", outcome: "prestart", attempt: 1, execs: 0},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			r := newRec(t)
			r.e.addJob(r.path, "j1", c.mode)
			r.crashAt(c.point, c.faults)
			if c.prepare != nil {
				c.prepare(r)
			}
			p := r.run(nil)
			p.waitReady(t)
			status, outcome := r.waitOutcome("j1")
			if c.outcome == "ok" {
				// a requeued attempt completes after the outcome of the first is set
				r.e.waitJob(r.path, "j1", "completed")
				status, outcome = r.e.jobOutcome(r.path, "j1")
			}
			if status != c.status || outcome != c.outcome {
				t.Fatalf("job is %s/%s, want %s/%s", status, outcome, c.status, c.outcome)
			}
			if got := r.crashes("j1"); got != c.crashes {
				t.Fatalf("crash_requeues = %d, want %d", got, c.crashes)
			}
			if got := r.e.count(r.path, `SELECT COUNT(*) FROM scheduler_job_attempts WHERE job_id = 'j1'`); got != c.attempt {
				t.Fatalf("attempts = %d, want %d", got, c.attempt)
			}
			if got := r.e.count(r.path, `SELECT COUNT(*) FROM dispatch_terminals`); got > 1 {
				t.Fatalf("terminal rows = %d, want at most 1", got)
			}
			if n := r.execs(); n != c.execs {
				t.Fatalf("execve of the executor = %d, want %d", n, c.execs)
			}
			// No second attempt appears later either.
			r.e.heartbeats(r.path, 2)
			if got := r.e.count(r.path, `SELECT COUNT(*) FROM scheduler_job_attempts WHERE job_id = 'j1'`); got != c.attempt {
				t.Fatalf("attempts after two more heartbeats = %d, want %d", got, c.attempt)
			}
			if c.check != nil {
				c.check(r)
			}
			r.stop(p)
		})
	}
}

// recordedChild is the PID the spawn-child mode wrote, or 0.
func (r *rec) recordedChild() int {
	child := 0
	for _, f := range r.childFiles() {
		b, _ := os.ReadFile(f)
		if n, _ := strconv.Atoi(strings.TrimSpace(string(b))); n > 0 {
			child = n
		}
	}
	return child
}

// liveChild is the recorded descendant's PID when that process exists.
func (r *rec) liveChild() int {
	if c := r.recordedChild(); c > 0 && !procGone(c) {
		return c
	}
	return 0
}

// childFiles lists the "<output>.child" files the spawn-child mode writes.
func (r *rec) childFiles() []string {
	var out []string
	_ = filepath.WalkDir(r.e.cwd, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".child") {
			out = append(out, p)
		}
		return nil
	})
	return out
}
