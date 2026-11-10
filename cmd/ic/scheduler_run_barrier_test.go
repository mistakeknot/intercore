//go:build icsched_test && linux

package main

// Barrier-driven integration tests for `ic scheduler run` (plan section 9: A2
// barrier steps, A5, A5d, A6, I10, observer calibration). A runner is held at a
// barrier point on a FIFO, stopped or killed there, and the database is copied
// or replaced while it is held. Every runner runs under strace, so the launch
// count is observed from outside the process under test. As in the other
// integration tests, HOME is throwaway, the working directory is under the
// test's temp dir and the database is an explicit --db file.

import (
	"bytes"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// barrier is one FIFO barrier shared with a runner through its environment.
type barrier struct {
	t    *testing.T
	fifo string
}

func newBarrier(t *testing.T, dir string) *barrier {
	t.Helper()
	fifo := filepath.Join(dir, "barrier.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	return &barrier{t: t, fifo: fifo}
}

func (b *barrier) env(point string) []string {
	return []string{"IC_SCHED_BARRIER=" + point, "IC_SCHED_BARRIER_FIFO=" + b.fifo}
}

// wait blocks until the runner reports it reached point.
func (b *barrier) wait(point string) {
	b.t.Helper()
	deadline := time.Now().Add(waitLimit)
	for {
		data, _ := os.ReadFile(b.fifo + ".hit")
		for _, l := range strings.Split(string(data), "\n") {
			if p, _, _ := strings.Cut(l, " "); p == point {
				return
			}
		}
		if time.Now().After(deadline) {
			b.t.Fatalf("runner never reached %s (hit file: %q)", point, string(data))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// release lets the held runner continue. The write end opens only once the
// runner is reading, so a runner that was killed would leave this blocked: the
// open is bounded.
func (b *barrier) release() {
	b.t.Helper()
	done := make(chan error, 1)
	go func() {
		f, err := os.OpenFile(b.fifo, os.O_WRONLY, 0)
		if err == nil {
			f.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			b.t.Fatalf("release barrier: %v", err)
		}
	case <-time.After(waitLimit):
		b.t.Fatal("release barrier: the runner is not reading the FIFO")
	}
}

// vacuumCopy writes a VACUUM INTO copy of src to dst: a new inode with the same
// queue_id, leaving src and its sidecars untouched.
func vacuumCopy(t *testing.T, src, dst string) {
	t.Helper()
	raw, err := sql.Open("sqlite", src)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`VACUUM INTO '` + dst + `'`); err != nil {
		t.Fatalf("VACUUM INTO %s: %v", dst, err)
	}
}

// executorExecs counts successful execves of the fake executor over every log.
func executorExecs(t *testing.T, fake string, logs ...string) int {
	t.Helper()
	n := 0
	for _, l := range logs {
		b, err := os.ReadFile(l)
		if err != nil {
			continue // a runner that never started leaves no log
		}
		n += len(execLines(string(b), bashOf(fake)))
	}
	return n
}

func (e *env) signalRunner(path string, sig syscall.Signal) int {
	e.t.Helper()
	pid := e.runnerPID(path)
	if err := syscall.Kill(pid, sig); err != nil {
		e.t.Fatal(err)
	}
	return pid
}

func (e *env) jobOutcome(path, id string) (status, outcome string) {
	var o sql.NullString
	var s string
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		e.t.Fatal(err)
	}
	defer raw.Close()
	if err := raw.QueryRow(`SELECT status, outcome_class FROM scheduler_jobs WHERE id = ?`, id).Scan(&s, &o); err != nil {
		e.t.Fatal(err)
	}
	return s, o.String
}

// blockedRunner starts a second runner and returns its exit code and stderr:
// a runner that cannot take the locks exits within --lock-wait.
func (e *env) blockedRunner(strace, rel, log, fake string, extra ...string) (int, string) {
	e.t.Helper()
	args := append([]string{"--dispatch-sh=" + fake, "--lock-wait=300ms"}, extra...)
	b := e.tracedRunner(strace, rel, log, nil, args...)
	code := b.waitExit(e.t)
	return code, b.stderr()
}

// Observer calibration: the launch count must see one launch as 1 and a
// deliberate duplicate launch (the same executor started twice with the same -o
// path outside any runner) as 2. If it cannot, every count below is meaningless.
func TestCLIObserverCalibration(t *testing.T) {
	strace := needStrace(t)
	e := newEnv(t)
	fake := e.fakeScript()
	prompt := filepath.Join(e.cwd, "cal.prompt")
	if err := os.WriteFile(prompt, []byte("ok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(e.cwd, "cal-out")
	run := func(log string, times int) int {
		script := ""
		for i := 0; i < times; i++ {
			script += fmt.Sprintf("bash %s --to codex -C %s -o %s --prompt-file %s; ", fake, e.cwd, out, prompt)
		}
		var buf bytes.Buffer
		cmd := exec.Command(strace, "-f", "-qq", "-s", "4096", "-e", "trace=execve", "-o", log, "--", "bash", "-c", script)
		cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = e.cwd, e.environ(), &buf, &buf
		if err := cmd.Run(); err != nil {
			t.Fatalf("calibration run: %v: %s", err, buf.String())
		}
		return len(execLines(mustRead(t, log), bashOf(fake)))
	}
	if n := run(filepath.Join(e.cwd, "cal1.log"), 1); n != 1 {
		t.Fatalf("one known launch counted %d, want 1", n)
	}
	if n := run(filepath.Join(e.cwd, "cal2.log"), 2); n != 2 {
		t.Fatalf("a deliberate duplicate launch counted %d, want 2", n)
	}
}

func mustRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// copyScenario is A5 and A6: runner A is held at point with a job claimed, the
// database is copied with VACUUM INTO to a separate path, and a runner B on the
// copy cannot take over while A is present.
type copyScenario struct {
	e      *env
	strace string
	fake   string
	path   string
	logA   string
	a      *proc
	bar    *barrier
}

func startHeldRunner(t *testing.T, point string) *copyScenario {
	t.Helper()
	strace := needStrace(t)
	e := newEnv(t)
	s := &copyScenario{e: e, strace: strace, fake: e.fakeScript(), path: e.newDB("q.db"), logA: filepath.Join(e.cwd, "exec.a.log")}
	s.bar = newBarrier(t, e.cwd)
	e.addJob(s.path, "j1", "ok")
	s.a = e.tracedRunner(strace, "q.db", s.logA, s.bar.env(point), "--dispatch-sh="+s.fake)
	s.a.waitReady(t)
	s.bar.wait(point)
	return s
}

// B must not be able to run on the copy while A is held: it exits without a
// launch, and without moving the copy's epoch when checkEpoch is set, whether or
// not it asked to rebind. A5d cannot read the replaced file through SQLite (A's
// WAL no longer matches it), so it skips the epoch check.
func (s *copyScenario) refuseB(t *testing.T, copyRel string, withRebind, checkEpoch bool) {
	t.Helper()
	copyPath := filepath.Join(s.e.cwd, copyRel)
	var epoch int64
	if checkEpoch {
		epoch = s.e.epoch(copyPath)
	}
	extra := []string{}
	name := "plain"
	if withRebind {
		extra = []string{"--rebind-db", "--confirm-old-runner-dead"}
		name = "rebind"
	}
	log := filepath.Join(s.e.cwd, "exec.b."+name+".log")
	code, stderr := s.e.blockedRunner(s.strace, copyRel, log, s.fake, extra...)
	if code == 0 {
		t.Fatalf("runner B (%s) on the copy exited 0 while A is present: %s", name, stderr)
	}
	if checkEpoch && s.e.epoch(copyPath) != epoch {
		got := s.e.epoch(copyPath)
		t.Fatalf("runner B (%s) moved the copy's epoch %d -> %d: %s", name, epoch, got, stderr)
	}
	if n := executorExecs(t, s.fake, log); n != 0 {
		t.Fatalf("runner B (%s) launched the executor %d times", name, n)
	}
	t.Logf("runner B (%s): exit %d: %s", name, code, strings.TrimSpace(stderr))
}

func testCopyWhileHeld(t *testing.T, point string) {
	t.Run("resume", func(t *testing.T) {
		s := startHeldRunner(t, point)
		pid := s.e.runnerPID(s.path)
		if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGCONT) })
		if n := executorExecs(t, s.fake, s.logA); n != 0 {
			t.Fatalf("executor started %d times before the barrier was released", n)
		}
		vacuumCopy(t, s.path, filepath.Join(s.e.cwd, "b.db"))
		s.refuseB(t, "b.db", false, true)
		s.refuseB(t, "b.db", true, true)

		if err := syscall.Kill(pid, syscall.SIGCONT); err != nil {
			t.Fatal(err)
		}
		s.bar.release()
		s.e.waitJob(s.path, "j1", "completed")
		if code := s.e.stopRunner(s.a, s.path); code != 0 {
			t.Fatalf("runner A exit %d: %s", code, s.a.stderr())
		}
		if n := executorExecs(t, s.fake, s.logA); n != 1 {
			t.Fatalf("launches after resume = %d, want 1", n)
		}
	})
	t.Run("kill", func(t *testing.T) {
		s := startHeldRunner(t, point)
		pid := s.e.runnerPID(s.path)
		if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
			t.Fatal(err)
		}
		vacuumCopy(t, s.path, filepath.Join(s.e.cwd, "b.db"))
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		s.a.waitExit(t)
		if n := executorExecs(t, s.fake, s.logA); n != 0 {
			t.Fatalf("A launched %d times although it was killed before Start", n)
		}

		logB := filepath.Join(s.e.cwd, "exec.b.log")
		b := s.e.tracedRunner(s.strace, "b.db", logB, nil, "--dispatch-sh="+s.fake, "--rebind-db", "--confirm-old-runner-dead")
		b.waitReady(t)
		copyPath := filepath.Join(s.e.cwd, "b.db")
		status, outcome := s.e.jobOutcome(copyPath, "j1")
		if status != "quarantined" || outcome != "rebind_unknown" {
			t.Fatalf("in-flight row is %s/%s, want quarantined/rebind_unknown", status, outcome)
		}
		var paused string
		s.e.query(copyPath, `SELECT payload FROM state WHERE key = 'scheduler_paused' AND scope_id = 'global'`, &paused)
		if paused != "true" {
			t.Fatalf("scheduler_paused = %q after rebind", paused)
		}
		// Give a runner that wrongly launched time to do so: it ticks every 20 ms.
		s.e.heartbeats(copyPath, 2)
		if code := s.e.stopRunner(b, copyPath); code != 0 {
			t.Fatalf("runner B exit %d: %s", code, b.stderr())
		}
		if n := executorExecs(t, s.fake, s.logA, logB); n != 0 {
			t.Fatalf("launches after the kill and rebind = %d, want 0", n)
		}
	})
}

// A5: held at after_claim.
func TestCLICopiedDBWhileClaimed(t *testing.T) { testCopyWhileHeld(t, "after_claim") }

// A6: held at after_link_before_start.
func TestCLICopiedDBWhileLinked(t *testing.T) { testCopyWhileHeld(t, "after_link_before_start") }

// A5d (destructive): the copy is renamed over A's open database path. SQLite
// does not support that, so the only acceptance is safety: B is refused or
// blocks, and there is at most one launch.
func TestCLIReplacedDBStaysSafe(t *testing.T) {
	s := startHeldRunner(t, "after_link_before_start")
	pid := s.e.runnerPID(s.path)
	if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	tmp := filepath.Join(s.e.cwd, "replacement.db")
	vacuumCopy(t, s.path, tmp)
	if err := os.Rename(tmp, s.path); err != nil {
		t.Fatal(err)
	}
	s.refuseB(t, "q.db", false, false)
	s.refuseB(t, "q.db", true, false)

	if err := syscall.Kill(pid, syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	s.bar.release()
	// A's later behaviour on the replaced file is not asserted. Let it run until
	// its launch shows in the trace or a short while passes, then stop it.
	deadline := time.Now().Add(5 * time.Second)
	for executorExecs(t, s.fake, s.logA) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGTERM)
	select {
	case <-s.a.done:
	case <-time.After(waitLimit):
		_ = syscall.Kill(pid, syscall.SIGKILL)
		<-s.a.done
	}
	if n := executorExecs(t, s.fake, s.logA); n > 1 {
		t.Fatalf("launches = %d, want at most 1", n)
	}
}

// A2 (barrier steps): runner A is held at after_link_before_start with a job
// linked. A runner B on a file bind mount of the database inside `unshare -Urm`
// cannot run and never moves the epoch; resuming A gives exactly one launch;
// once A has exited, B is refused by the mount-point check.
func TestCLIBindMountAliasWhileLinked(t *testing.T) {
	userNS(t, "-Urm")
	s := startHeldRunner(t, "after_link_before_start")
	e := s.e
	pid := e.runnerPID(s.path)
	if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGCONT) })
	epoch := e.epoch(s.path)

	alias := filepath.Join(e.cwd, "bound.db")
	if err := os.WriteFile(alias, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	inNS := func(script string, env ...string) (string, error) {
		cmd := exec.Command("unshare", "-Urm", "sh", "-c", script)
		cmd.Dir = e.cwd
		cmd.Env = append(e.environ(), env...)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		return out.String(), err
	}
	if out, err := inNS(`mount --bind q.db bound.db`); err != nil {
		t.Skipf("file bind mounts unavailable in a user namespace: %v: %s", err, out)
	}

	// B on the bind mount: refused without a launch and without moving the epoch.
	script := fmt.Sprintf(`mount --bind q.db bound.db && exec %s --db=bound.db scheduler run --lock-wait=300ms --no-containment`, e.bin)
	out, err := inNS(script)
	ee, ok := err.(*exec.ExitError)
	if !ok || ee.ExitCode() == 0 {
		t.Fatalf("runner B on a bind mount did not exit non-zero: %v: %s", err, out)
	}
	t.Logf("runner B (A suspended): exit %d: %s", ee.ExitCode(), strings.TrimSpace(out))
	if got := e.epoch(s.path); got != epoch {
		t.Fatalf("epoch moved %d -> %d", epoch, got)
	}
	if n := executorExecs(t, s.fake, s.logA); n != 0 {
		t.Fatalf("executor launched %d times while A was suspended before Start", n)
	}

	if err := syscall.Kill(pid, syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	s.bar.release()
	e.waitJob(s.path, "j1", "completed")
	if code := e.stopRunner(s.a, s.path); code != 0 {
		t.Fatalf("runner A exit %d: %s", code, s.a.stderr())
	}
	if n := executorExecs(t, s.fake, s.logA); n != 1 {
		t.Fatalf("launches after resume = %d, want 1", n)
	}

	// With A gone the bind mount is still refused, by the mount-point check.
	epoch = e.epoch(s.path)
	out, err = inNS(script)
	ee, ok = err.(*exec.ExitError)
	if !ok || ee.ExitCode() != 1 || !strings.Contains(out, "mount point") {
		t.Fatalf("runner B after A exited: %v: %s", err, out)
	}
	if got := e.epoch(s.path); got != epoch {
		t.Fatalf("epoch moved %d -> %d", epoch, got)
	}
}

// I10: fifty queued fast jobs all complete, each is launched exactly once, and
// the time to claim and finish them is reported.
func TestCLIFiftyFastJobs(t *testing.T) {
	strace := needStrace(t)
	e := newEnv(t)
	fake := e.fakeScript()
	path := e.newDB("q.db")
	const n = 50
	for i := 0; i < n; i++ {
		e.addJob(path, fmt.Sprintf("j%02d", i), "ok")
	}
	log := filepath.Join(e.cwd, "exec.log")
	p := e.tracedRunner(strace, "q.db", log, nil, "--dispatch-sh="+fake, "--max-concurrent=4")
	p.waitReady(t)
	start := time.Now()
	for i := 0; i < n; i++ {
		e.waitJob(path, fmt.Sprintf("j%02d", i), "completed")
	}
	elapsed := time.Since(start)
	if code := e.stopRunner(p, path); code != 0 {
		t.Fatalf("runner exit %d: %s", code, p.stderr())
	}
	if got := e.count(path, `SELECT COUNT(*) FROM scheduler_job_attempts`); got != n {
		t.Fatalf("attempts = %d, want %d", got, n)
	}
	if got := e.count(path, `SELECT COUNT(*) FROM dispatches`); got != n {
		t.Fatalf("dispatches = %d, want %d", got, n)
	}
	if got := executorExecs(t, fake, log); got != n {
		t.Fatalf("executor launches = %d, want %d", got, n)
	}
	t.Logf("I10: %d jobs claimed, launched and completed in %s (%s per job, max-concurrent 4, under strace)",
		n, elapsed.Round(time.Millisecond), (elapsed / n).Round(time.Millisecond))
}
