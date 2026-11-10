//go:build icsched_test && linux

package main

// Integration tests for the claim and launch loop through the real
// icsched_test binary (plan section 9: I4, I6, I8, G1, X1). The runner runs
// under strace so every execve is observed from outside the process under
// test; a test skips, with the reason, when strace cannot trace here. As in the
// other integration tests, HOME is throwaway, the working directory is under
// the test's temp dir and the database is an explicit --db file.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mistakeknot/intercore/internal/db"
	"github.com/mistakeknot/intercore/internal/scheduler"
)

// needStrace skips unless strace can trace an execve here. The calibration is a
// positive control: a command that is known to exec must show up in the log.
func needStrace(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("strace")
	if err != nil {
		t.Skipf("strace not installed: %v", err)
	}
	log := filepath.Join(t.TempDir(), "calibration")
	if out, err := exec.Command(p, "-f", "-qq", "-e", "trace=execve", "-o", log, "true").CombinedOutput(); err != nil {
		t.Skipf("strace cannot trace here (%v: %s)", err, strings.TrimSpace(string(out)))
	}
	b, _ := os.ReadFile(log)
	if !strings.Contains(string(b), "execve(") {
		t.Skipf("strace ran but recorded no execve; calibration log: %q", string(b))
	}
	return p
}

var (
	execOK         = regexp.MustCompile(`^(\d+) +execve\(.*\) = 0$`)
	execUnfinished = regexp.MustCompile(`^(\d+) +execve\(.*<unfinished \.\.\.>$`)
	execResumedOK  = regexp.MustCompile(`^(\d+) +<\.\.\. execve resumed>.*= 0$`)
)

// execLines returns the successful execve lines of a strace log that contain
// needle. With -f, strace splits a call into "<unfinished ...>" and
// "<... execve resumed>" lines when another tracee prints in between, so a
// split call counts once, on its first line, if and only if it resumed with 0.
func execLines(log, needle string) []string {
	var out []string
	pending := map[string]string{} // pid -> unfinished line that contains needle
	for _, line := range strings.Split(log, "\n") {
		switch {
		case execOK.MatchString(line):
			if strings.Contains(line, needle) {
				out = append(out, line)
			}
		case execUnfinished.MatchString(line):
			if strings.Contains(line, needle) {
				pending[execUnfinished.FindStringSubmatch(line)[1]] = line
			}
		case execResumedOK.MatchString(line):
			pid := execResumedOK.FindStringSubmatch(line)[1]
			if l, ok := pending[pid]; ok {
				out = append(out, l)
				delete(pending, pid)
			}
		}
	}
	return out
}

// countExecs counts successful execve calls in a strace log whose line
// contains needle. When the count is not the one a test expects, the lines
// that mention the needle at all are the first thing to read, so a failure
// should call dumpExecs.
func countExecs(t *testing.T, log, needle string) int {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	lines := execLines(string(b), needle)
	for _, l := range lines {
		t.Logf("execve: %.300s", l)
	}
	if len(lines) == 0 {
		for _, l := range strings.Split(string(b), "\n") {
			if strings.Contains(l, needle) || strings.Contains(l, "execve resumed") {
				t.Logf("trace line without a counted execve: %.300s", l)
			}
		}
	}
	return len(lines)
}

// bashOf is what an execve of the executor looks like in a strace log: the
// runner starts it as `bash <script> ...`. The runner's own execve line also
// carries the script path (as --dispatch-sh), so the path alone would count it.
func bashOf(script string) string { return `"bash", "` + script + `"` }

func (e *env) fakeScript() string {
	e.t.Helper()
	p, err := filepath.Abs("../../internal/scheduler/runner/testdata/fake-dispatch.sh")
	if err != nil {
		e.t.Fatal(err)
	}
	if err := os.Chmod(p, 0o755); err != nil {
		e.t.Fatal(err)
	}
	return p
}

// tracedRunner starts `ic scheduler run` as a strace child. extraEnv adds to
// the throwaway environment.
func (e *env) tracedRunner(strace, rel, tracelog string, extraEnv []string, extra ...string) *proc {
	e.t.Helper()
	args := []string{"-f", "-qq", "-s", "4096", "-e", "trace=execve", "-o", tracelog, "--", e.bin,
		"--db=" + rel, "scheduler", "run", "--heartbeat-interval=50ms", "--tick=20ms"}
	cmd := exec.Command(strace, append(args, extra...)...)
	cmd.Dir = e.cwd
	cmd.Env = append(e.environ(), extraEnv...)
	p := launch(e.t, cmd)
	// A failed test must not leave a runner (or the strace around it) behind:
	// a leaked child keeps go test waiting on its output pipe. strace would
	// detach, not kill, so the runner is killed by the pid it recorded.
	e.t.Cleanup(func() {
		select {
		case <-p.done:
			return
		default:
		}
		var pid int
		if raw, err := sql.Open("sqlite", filepath.Join(e.cwd, rel)); err == nil {
			_ = raw.QueryRow(`SELECT COALESCE(pid, 0) FROM scheduler_runner WHERE id = 1`).Scan(&pid)
			raw.Close()
		}
		if pid > 1 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		_ = p.cmd.Process.Kill()
	})
	return p
}

// runnerPID reads the pid the runner recorded for itself.
func (e *env) runnerPID(path string) int {
	e.t.Helper()
	var pid int
	e.query(path, `SELECT pid FROM scheduler_runner WHERE id = 1`, &pid)
	return pid
}

// stopRunner sends SIGTERM to the runner itself (its parent is strace, which
// would detach instead of relaying) and returns the exit code strace reports.
func (e *env) stopRunner(p *proc, path string) int {
	e.t.Helper()
	if err := syscall.Kill(e.runnerPID(path), syscall.SIGTERM); err != nil {
		e.t.Fatal(err)
	}
	return p.waitExit(e.t)
}

// ic runs the binary to completion and returns stdout, stderr and the exit code.
func (e *env) ic(args ...string) (string, string, int) {
	e.t.Helper()
	cmd := exec.Command(e.bin, args...)
	cmd.Dir = e.cwd
	cmd.Env = e.environ()
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			e.t.Fatal(err)
		}
		code = ee.ExitCode()
	}
	return so.String(), se.String(), code
}

// addJob inserts a pending job the way a submitter that sets an output base
// does, with the prompt's first line picking the fake executor's behaviour.
func (e *env) addJob(path, id, mode string) {
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
	err = scheduler.NewStore(d.SqlDB()).Create(context.Background(), &scheduler.SpawnJob{
		ID: id, Status: scheduler.StatusPending, Priority: scheduler.PriorityNormal, AgentType: "codex",
		SpawnOpts: string(so), CreatedAt: time.Unix(1_800_000_000, 0),
		ProjectDir: e.cwd, OutputBase: filepath.Join(e.cwd, "out-"+id),
	})
	if err != nil {
		e.t.Fatalf("create %s: %v", id, err)
	}
}

func (e *env) jobStatus(path, id string) string {
	var s string
	e.query(path, `SELECT status FROM scheduler_jobs WHERE id = '`+id+`'`, &s)
	return s
}

func (e *env) waitJob(path, id string, want ...string) string {
	e.t.Helper()
	deadline := time.Now().Add(waitLimit)
	for {
		s := e.jobStatus(path, id)
		for _, w := range want {
			if s == w {
				return s
			}
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("job %s is %s, wanted %v", id, s, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (e *env) count(path, q string) int {
	var n int
	e.query(path, q, &n)
	return n
}

// I4: a job runs to completion with one attempt, one dispatch and exactly one
// observed execve of the executor, and leaves no zombie.
func TestCLIHappyPathOneExecve(t *testing.T) {
	strace := needStrace(t)
	e := newEnv(t)
	fake := e.fakeScript()
	path := e.newDB("q.db")
	e.addJob(path, "j1", "ok")
	log := filepath.Join(e.cwd, "trace.log")

	p := e.tracedRunner(strace, "q.db", log, nil, "--dispatch-sh="+fake)
	p.waitReady(t)
	e.waitJob(path, "j1", "completed")
	var pid int
	e.query(path, `SELECT COALESCE(pid, 0) FROM dispatches LIMIT 1`, &pid)
	if code := e.stopRunner(p, path); code != 0 {
		t.Fatalf("runner exit %d: %s", code, p.stderr())
	}

	if n := e.count(path, `SELECT COUNT(*) FROM scheduler_job_attempts WHERE job_id = 'j1'`); n != 1 {
		t.Fatalf("attempts = %d", n)
	}
	if n := e.count(path, `SELECT COUNT(*) FROM dispatches`); n != 1 {
		t.Fatalf("dispatches = %d", n)
	}
	if n := countExecs(t, log, bashOf(fake)); n != 1 {
		t.Fatalf("execve of the executor = %d, want 1", n)
	}
	if pid > 0 {
		if b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil {
			if f := strings.Fields(string(b)); len(f) > 2 && f[2] == "Z" {
				t.Fatalf("dispatch pid %d is a zombie", pid)
			}
		}
	}
}

// G1: a governed job inserted by the current (old-client) `ic dispatch spawn
// --scheduled --role` fails config_error governed_unsupported with no attempt,
// no dispatch and no execve. A plain job in the same run is the control: the
// same observer sees its single execve.
func TestCLIGovernedJobFailsWithoutExec(t *testing.T) {
	strace := needStrace(t)
	e := newEnv(t)
	fake := e.fakeScript()
	path := e.newDB("q.db")

	policy := filepath.Join(e.cwd, "plugin", "config", "routing.yaml")
	if err := os.MkdirAll(filepath.Dir(policy), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("../../orchcompat/testdata/routing.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(policy, data, 0o644); err != nil {
		t.Fatal(err)
	}
	prompt := filepath.Join(e.cwd, "gov.prompt")
	if err := os.WriteFile(prompt, []byte("ok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := e.ic("--db=q.db", "--json", "dispatch", "spawn", "--policy="+policy, "--role=planning",
		"--prompt-file="+prompt, "--project="+e.cwd, "--scheduled")
	if code != 0 {
		t.Fatalf("old-client spawn exit %d: %s %s", code, out, errOut)
	}
	var sub struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal([]byte(out), &sub); err != nil || sub.JobID == "" {
		t.Fatalf("spawn output %q: %v", out, err)
	}
	e.addJob(path, "control", "ok")
	log := filepath.Join(e.cwd, "trace.log")

	p := e.tracedRunner(strace, "q.db", log, nil, "--dispatch-sh="+fake)
	p.waitReady(t)
	e.waitJob(path, sub.JobID, "failed")
	e.waitJob(path, "control", "completed")
	if code := e.stopRunner(p, path); code != 0 {
		t.Fatalf("runner exit %d: %s", code, p.stderr())
	}

	var class, msg string
	e.query(path, `SELECT outcome_class, error_msg FROM scheduler_jobs WHERE id = '`+sub.JobID+`'`, &class, &msg)
	if class != scheduler.ClassConfigError || !strings.HasPrefix(msg, "governed_unsupported:") {
		t.Fatalf("governed job: class %q, error %q", class, msg)
	}
	if n := e.count(path, `SELECT COUNT(*) FROM scheduler_job_attempts WHERE job_id = '`+sub.JobID+`'`); n != 0 {
		t.Fatalf("governed job has %d attempts", n)
	}
	if n := e.count(path, `SELECT COUNT(*) FROM dispatches`); n != 1 {
		t.Fatalf("dispatch rows = %d, want only the control's", n)
	}
	if n := countExecs(t, log, bashOf(fake)); n != 1 {
		t.Fatalf("execve of the executor = %d, want 1 (the control)", n)
	}
}

// X1: the executor is only what the runner was told. With no --dispatch-sh and
// no CLAVAIN_DISPATCH_SH, sentinels on PATH and at <cwd>/os/Clavain/scripts are
// never run, and the job fails config_error no_executor. The control restarts
// the runner with the env variable set: the same job type then runs the fake.
func TestCLIExecutorIsNeverSubstituted(t *testing.T) {
	strace := needStrace(t)
	e := newEnv(t)
	fake := e.fakeScript()
	path := e.newDB("q.db")

	marker := filepath.Join(e.cwd, "sentinel-ran")
	sentinel := "#!/bin/sh\n: > " + marker + "\nexit 0\n"
	pathSentinel := filepath.Join(e.cwd, "sentbin", "dispatch.sh")
	cwdSentinel := filepath.Join(e.cwd, "os", "Clavain", "scripts", "dispatch.sh")
	for _, p := range []string{pathSentinel, cwdSentinel} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(sentinel), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	sentEnv := []string{"PATH=" + filepath.Dir(pathSentinel) + ":" + os.Getenv("PATH")}

	e.addJob(path, "x1", "ok")
	log1 := filepath.Join(e.cwd, "trace1.log")
	p := e.tracedRunner(strace, "q.db", log1, sentEnv)
	p.waitReady(t)
	e.waitJob(path, "x1", "failed")
	if code := e.stopRunner(p, path); code != 0 {
		t.Fatalf("runner exit %d: %s", code, p.stderr())
	}
	var class, msg string
	e.query(path, `SELECT outcome_class, error_msg FROM scheduler_jobs WHERE id = 'x1'`, &class, &msg)
	if class != scheduler.ClassConfigError || !strings.HasPrefix(msg, "no_executor:") {
		t.Fatalf("x1: class %q, error %q", class, msg)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a sentinel executor ran")
	}
	for _, needle := range []string{"dispatch.sh", "/sentbin/", "/os/Clavain/"} {
		if n := countExecs(t, log1, needle); n != 0 {
			t.Fatalf("execve matching %q = %d, want 0", needle, n)
		}
	}
	if n := e.count(path, `SELECT COUNT(*) FROM dispatches`); n != 0 {
		t.Fatalf("dispatch rows = %d", n)
	}

	// Control: the env variable is honoured, so the observer does see an exec.
	e.addJob(path, "x2", "ok")
	log2 := filepath.Join(e.cwd, "trace2.log")
	p2 := e.tracedRunner(strace, "q.db", log2, append(sentEnv, "CLAVAIN_DISPATCH_SH="+fake))
	p2.waitReady(t)
	e.waitJob(path, "x2", "completed")
	if code := e.stopRunner(p2, path); code != 0 {
		t.Fatalf("runner exit %d: %s", code, p2.stderr())
	}
	if n := countExecs(t, log2, bashOf(fake)); n != 1 {
		t.Fatalf("control: execve of the executor = %d, want 1", n)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a sentinel executor ran in the control run")
	}
}

// heartbeats waits until the runner's heartbeat has moved to n distinct values
// past its current one, i.e. until the runner has demonstrably kept running.
func (e *env) heartbeats(path string, n int) {
	e.t.Helper()
	var last int64
	e.query(path, `SELECT heartbeat_at FROM scheduler_runner WHERE id = 1`, &last)
	deadline := time.Now().Add(waitLimit)
	for seen := 0; seen < n; {
		var hb int64
		e.query(path, `SELECT heartbeat_at FROM scheduler_runner WHERE id = 1`, &hb)
		if hb != last {
			last, seen = hb, seen+1
		}
		if time.Now().After(deadline) {
			e.t.Fatal("heartbeat did not advance")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// I6: a pause set with `ic scheduler pause` survives a runner restart. The new
// runner keeps heartbeating (so it has run many claim passes) and still claims
// nothing; after `resume` the same job runs once.
func TestCLIPauseSurvivesRestart(t *testing.T) {
	strace := needStrace(t)
	e := newEnv(t)
	fake := e.fakeScript()
	path := e.newDB("q.db")

	a := e.tracedRunner(strace, "q.db", filepath.Join(e.cwd, "trace-a.log"), nil, "--dispatch-sh="+fake)
	a.waitReady(t)
	if _, se, code := e.ic("--db=q.db", "scheduler", "pause"); code != 0 {
		t.Fatalf("pause exit %d: %s", code, se)
	}
	if code := e.stopRunner(a, path); code != 0 {
		t.Fatalf("runner A exit %d: %s", code, a.stderr())
	}

	e.addJob(path, "held", "ok")
	log := filepath.Join(e.cwd, "trace-b.log")
	b := e.tracedRunner(strace, "q.db", log, nil, "--dispatch-sh="+fake)
	b.waitReady(t)
	e.heartbeats(path, 2)
	if s := e.jobStatus(path, "held"); s != "pending" {
		t.Fatalf("job is %s while paused after a restart", s)
	}
	if n := countExecs(t, log, bashOf(fake)); n != 0 {
		t.Fatalf("execve while paused = %d", n)
	}
	out, _, _ := e.ic("--db=q.db", "--json", "scheduler", "runner")
	var st struct {
		Paused bool `json:"paused"`
	}
	if err := json.Unmarshal([]byte(out), &st); err != nil || !st.Paused {
		t.Fatalf("runner status %q does not report paused (%v)", out, err)
	}

	// Control: resume lets the same runner claim the same job.
	if _, se, code := e.ic("--db=q.db", "scheduler", "resume"); code != 0 {
		t.Fatalf("resume exit %d: %s", code, se)
	}
	e.waitJob(path, "held", "completed")
	if code := e.stopRunner(b, path); code != 0 {
		t.Fatalf("runner B exit %d: %s", code, b.stderr())
	}
	if n := countExecs(t, log, bashOf(fake)); n != 1 {
		t.Fatalf("execve after resume = %d, want 1", n)
	}
}

// runnerWord runs `ic scheduler runner --json` and returns its verdict.
func (e *env) runnerWord(stale string) string {
	e.t.Helper()
	out, se, code := e.ic("--db=q.db", "--json", "scheduler", "runner", "--stale-after="+stale)
	if code != 0 {
		e.t.Fatalf("scheduler runner exit %d: %s", code, se)
	}
	var st struct {
		Runner string `json:"runner"`
	}
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		e.t.Fatalf("scheduler runner output %q: %v", out, err)
	}
	return st.Runner
}

// I8: `ic scheduler runner` says alive for a live runner, hung for one that is
// SIGSTOP'd (the process exists, the heartbeat goes stale) and absent once the
// runner has exited. A database no runner ever served is absent too.
func TestCLIRunnerLiveness(t *testing.T) {
	e := newEnv(t)
	path := e.newDB("q.db")

	if w := e.runnerWord("2s"); w != "absent" {
		t.Fatalf("never-run database: %s, want absent", w)
	}

	r := e.runner("q.db")
	t.Cleanup(func() {
		select {
		case <-r.done:
		default:
			_ = r.cmd.Process.Signal(syscall.SIGCONT)
			_ = r.cmd.Process.Kill()
		}
	})
	r.waitReady(t)
	if w := e.runnerWord("2s"); w != "alive" {
		t.Fatalf("live runner: %s, want alive", w)
	}
	out, _, _ := e.ic("--db=q.db", "scheduler", "runner")
	if first := strings.SplitN(out, "\n", 2)[0]; first != "alive" {
		t.Fatalf("text output starts %q, want alive", first)
	}

	pid := e.runnerPID(path)
	if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	resumed := false
	defer func() {
		if !resumed {
			_ = syscall.Kill(pid, syscall.SIGCONT)
		}
	}()
	deadline := time.Now().Add(waitLimit)
	for e.runnerWord("2s") != "hung" {
		if time.Now().After(deadline) {
			t.Fatal("a stopped runner never reported hung")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := syscall.Kill(pid, syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	resumed = true

	if code := r.stop(t); code != 0 {
		t.Fatalf("runner exit %d: %s", code, r.stderr())
	}
	if w := e.runnerWord("2s"); w != "absent" {
		t.Fatalf("exited runner: %s, want absent", w)
	}
}

// The probe is read-only. A missing database is an error and is not created;
// a rollback-journal database keeps its journal mode.
func TestCLIRunnerProbeIsReadOnly(t *testing.T) {
	e := newEnv(t)

	_, se, code := e.ic("--db=missing.db", "scheduler", "runner")
	if code != 2 {
		t.Fatalf("probe of a missing database: exit %d (%s), want 2", code, se)
	}
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(filepath.Join(e.cwd, "missing.db"+suffix)); err == nil {
			t.Fatalf("the probe created missing.db%s", suffix)
		}
	}

	path := e.newDB("q.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	var mode string
	if err := raw.QueryRow("PRAGMA journal_mode = DELETE").Scan(&mode); err != nil || mode != "delete" {
		t.Fatalf("could not set the rollback journal: %q, %v", mode, err)
	}
	raw.Close()

	if w := e.runnerWord("2s"); w != "absent" {
		t.Fatalf("probe of a rollback-journal database: %s, want absent", w)
	}
	raw, err = sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if err := raw.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "delete" {
		t.Fatalf("the probe changed the journal mode to %q (%v)", mode, err)
	}
}

// A runner that crashed leaves committed frames in the WAL. Probing must not
// fold them into the database file or remove the WAL: the file and its WAL are
// byte-for-byte what the crash left.
func TestCLIRunnerProbeLeavesACrashedWALAlone(t *testing.T) {
	e := newEnv(t)
	live := e.newDB("live.db")
	raw, err := sql.Open("sqlite", "file:"+live)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	raw.SetMaxOpenConns(1)
	for _, q := range []string{"PRAGMA journal_mode = WAL", "PRAGMA wal_autocheckpoint = 0",
		"CREATE TABLE crash_marker (x INTEGER)", "INSERT INTO crash_marker VALUES (1)"} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// The snapshot is the on-disk state at the moment of a crash: the main
	// file and a WAL holding a committed transaction not yet checkpointed.
	for _, suffix := range []string{"", "-wal"} {
		b, err := os.ReadFile(live + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(e.cwd, "crash.db"+suffix), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	snap := func(suffix string) []byte {
		b, err := os.ReadFile(filepath.Join(e.cwd, "crash.db"+suffix))
		if err != nil {
			t.Fatalf("crash.db%s: %v", suffix, err)
		}
		return b
	}
	dbBefore, walBefore := snap(""), snap("-wal")
	if len(walBefore) == 0 {
		t.Fatal("the snapshot has no WAL frames, so the test proves nothing")
	}

	_, se, code := e.ic("--db=crash.db", "scheduler", "runner")
	if code != 0 {
		t.Fatalf("probe of a crashed database: exit %d: %s", code, se)
	}
	if !bytes.Equal(dbBefore, snap("")) {
		t.Fatal("the probe changed the database file")
	}
	if !bytes.Equal(walBefore, snap("-wal")) {
		t.Fatal("the probe changed or removed the WAL")
	}
}

// TestExecLinesHandlesSplitCalls is the parser's own check, with no runner: a
// call that strace split across two lines counts once when it succeeded and
// never when it failed, and a failed or unrelated call never counts.
func TestExecLinesHandlesSplitCalls(t *testing.T) {
	log := strings.Join([]string{
		`10 execve("/usr/bin/bash", ["bash", "/x/fake.sh", "a"], 0x1 /* 3 vars */) = 0`,
		`11 execve("/usr/bin/bash", ["bash", "/x/fake.sh", "b"], 0x1 /* 3 vars */ <unfinished ...>`,
		`12 write(2, "x", 1) = 1`,
		`11 <... execve resumed>) = 0`,
		`13 execve("/usr/bin/bash", ["bash", "/x/fake.sh", "c"], 0x1 /* 3 vars */ <unfinished ...>`,
		`13 <... execve resumed>) = -1 ENOENT (No such file or directory)`,
		`14 execve("/usr/bin/bash", ["bash", "/x/fake.sh", "d"], 0x1) = -1 EACCES (Permission denied)`,
		`15 execve("/usr/bin/bash", ["bash", "/x/other.sh"], 0x1) = 0`,
		`16 execve("/usr/bin/bash", ["bash", "/x/other.sh"], 0x1 <unfinished ...>`,
		`16 <... execve resumed>) = 0`,
	}, "\n")
	got := execLines(log, bashOf("/x/fake.sh"))
	if len(got) != 2 || !strings.Contains(got[0], `"a"`) || !strings.Contains(got[1], `"b"`) {
		t.Fatalf("counted %d lines: %q", len(got), got)
	}
}
