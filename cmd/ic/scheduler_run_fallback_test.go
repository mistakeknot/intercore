//go:build icsched_test && linux

package main

// Slice 4d through the real icsched_test binary: the fallback-mode (no
// containment) subset of the kill matrix, P2, P3 and P3b. In fallback mode the
// runner has no attempt cgroup, so it can neither prove a worker gone nor kill
// descendants it cannot name. It never adopts a process it found by searching;
// it SIGKILLs the same-uid carriers of IC_DISPATCH_ID through pidfds, ends the
// dispatch indeterminate and sets retry_blocked=survivor_unknown:<dispatch id>.
// Only `ic scheduler unblock <id> --confirm-not-running` lets the job run again.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mistakeknot/intercore/internal/scheduler"
)

// carriers lists the pids of every live process whose environment carries
// IC_DISPATCH_ID=<id>, read from the real /proc.
func carriers(did string) []int {
	want := "IC_DISPATCH_ID=" + did
	ents, _ := os.ReadDir("/proc")
	var out []int
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 || procGone(pid) {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/environ")
		if err != nil {
			continue
		}
		for _, kv := range strings.Split(string(b), "\x00") {
			if kv == want {
				out = append(out, pid)
				break
			}
		}
	}
	return out
}

// killCarriers kills every carrier of the dispatch id and waits until all are
// gone, like workers that died with the runner.
func killCarriers(t *testing.T, did string) {
	t.Helper()
	for _, pid := range carriers(did) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	deadline := time.Now().Add(10 * time.Second)
	for len(carriers(did)) > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("carriers of %s still alive: %v", did, carriers(did))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (r *rec) dispatchID() string { return r.str(`SELECT id FROM dispatches LIMIT 1`) }

func (r *rec) blocked(id string) string {
	return r.str(`SELECT retry_blocked FROM scheduler_jobs WHERE id = '` + id + `'`)
}

// waitBlocked waits for the job's retry_blocked marker to be set.
func (r *rec) waitBlocked(id string) string {
	r.t.Helper()
	deadline := time.Now().Add(waitLimit)
	for {
		if b := r.blocked(id); b != "" {
			return b
		}
		if time.Now().After(deadline) {
			st, oc := r.e.jobOutcome(r.path, id)
			r.t.Fatalf("job %s never got retry_blocked (status %s outcome %s)", id, st, oc)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

const retryIndeterminate = `{"max_attempts":2,"backoff":{"base_s":0.05,"factor":1,"max_s":0.05,"jitter":0},"retry_on":["indeterminate"]}`

func idempotentRetry(j *scheduler.SpawnJob) {
	j.Idempotent = true
	j.RetryPolicy = retryIndeterminate
}

var sweepKilled = regexp.MustCompile(`fallback sweep .*found (\d+), killed (\d+), unproven (\d+)`)

// The fallback subset of the kill matrix (barriers 3 to 6 with
// --no-containment): every case whose worker cannot be proven gone ends
// indeterminate with retry_blocked set, even when the sweep finds nothing, and
// a barrier-5 worker that is still alive is adopted as before.
func TestCLIFallbackKillMatrix(t *testing.T) {
	type tc struct {
		name    string
		point   string
		mode    string
		retry   bool // an indeterminate retry opt-in: the job must still not run again
		prepare func(r *rec, did string)
		blocked bool   // retry_blocked must be survivor_unknown:<did>
		status  string // end status when not retried
		outcome string
		execs   int
		check   func(r *rec, p *proc, did string)
	}
	waitChild := func(r *rec) {
		for i := 0; i < 500 && r.liveChild() == 0; i++ {
			time.Sleep(10 * time.Millisecond)
		}
		if r.liveChild() == 0 {
			r.t.Fatalf("the executor never started a live descendant")
		}
	}
	cases := []tc{
		{name: "b3_after_link_before_start", point: "after_link_before_start", mode: "ok", blocked: true, status: "failed", outcome: "indeterminate", execs: 0},
		{name: "b4_alive_sleep", point: "after_start_before_identity", mode: "sleep 20", blocked: true, status: "failed", outcome: "indeterminate", execs: 1,
			check: func(r *rec, p *proc, did string) {
				if n := len(carriers(did)); n != 0 {
					r.t.Fatalf("%d carriers of the dispatch survived the sweep", n)
				}
				pid := execPIDs(r.t, r.logs[0], bashOf(r.fake))[0]
				waitProcGone(r.t, pid, "the executor")
			}},
		// P2: every carrier of IC_DISPATCH_ID is killed through a pidfd and none
		// is adopted.
		{name: "b4_spawn_child_P2", point: "after_start_before_identity", mode: "spawn-child", blocked: true, status: "failed", outcome: "indeterminate", execs: 1,
			prepare: func(r *rec, did string) { waitChild(r) },
			check: func(r *rec, p *proc, did string) {
				child := r.recordedChild()
				if child <= 0 {
					r.t.Fatalf("no descendant pid recorded")
				}
				waitProcGone(r.t, child, "the executor's descendant")
				if n := len(carriers(did)); n != 0 {
					r.t.Fatalf("%d carriers of the dispatch survived the sweep", n)
				}
				m := sweepKilled.FindStringSubmatch(p.stderr())
				if m == nil {
					r.t.Fatalf("no sweep line in the log:\n%s", p.stderr())
				}
				if k, _ := strconv.Atoi(m[2]); k < 1 {
					r.t.Fatalf("the sweep killed %s processes, want at least 1:\n%s", m[2], p.stderr())
				}
			}},
		// The carriers are already gone: the sweep finds nothing and that still is
		// not proof of absence.
		{name: "b4_dead", point: "after_start_before_identity", mode: "sleep 20", blocked: true, status: "failed", outcome: "indeterminate", execs: 1,
			prepare: func(r *rec, did string) { killCarriers(r.t, did) },
			check: func(r *rec, p *proc, did string) {
				if m := sweepKilled.FindStringSubmatch(p.stderr()); m == nil || m[1] != "0" {
					r.t.Fatalf("expected a sweep that found nothing:\n%s", p.stderr())
				}
			}},
		// P3b: the descendant cleared its environment and left the session, so
		// the sweep cannot see it. The job is still blocked; the descendant is
		// not signalled by anything but the test cleanup.
		{name: "b4_setsid_clear_env_P3b", point: "after_start_before_identity", mode: "setsid-clear-env", blocked: true, status: "failed", outcome: "indeterminate", execs: 1,
			prepare: func(r *rec, did string) { waitChild(r) },
			check: func(r *rec, p *proc, did string) {
				if m := sweepKilled.FindStringSubmatch(p.stderr()); m == nil || m[1] != "0" || m[2] != "0" {
					r.t.Fatalf("the sweep must find nothing for a descendant with no environment:\n%s", p.stderr())
				}
				child := r.liveChild()
				if child == 0 {
					r.t.Fatalf("the environment-clearing descendant was killed by the sweep")
				}
				_ = syscall.Kill(child, syscall.SIGKILL)
				waitProcGone(r.t, child, "the environment-clearing descendant")
			}},
		// A barrier-6 runner dies after the launch result is stored. The wrapper
		// finished; its descendant is alive and nothing can prove it gone.
		{name: "b6_spawn_child", point: "after_launch_result_commit", mode: "spawn-child", retry: true, blocked: true, execs: 1,
			prepare: func(r *rec, did string) { waitChild(r) },
			check: func(r *rec, p *proc, did string) {
				child := r.recordedChild()
				waitProcGone(r.t, child, "the executor's descendant")
			}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			r := newRecNoCgroup(t)
			if c.retry {
				r.e.addJobWith(r.path, "j1", c.mode, idempotentRetry)
			} else {
				r.e.addJob(r.path, "j1", c.mode)
			}
			r.crashAt(c.point, "")
			did := r.dispatchID()
			if did == "" {
				t.Fatal("no dispatch row")
			}
			if c.prepare != nil {
				c.prepare(r, did)
			}
			p := r.run(nil)
			p.waitReady(t)
			got := r.waitBlocked("j1")
			if want := "survivor_unknown:" + did; got != want {
				t.Fatalf("retry_blocked = %q, want %q", got, want)
			}
			if c.retry {
				if st := r.e.jobStatus(r.path, "j1"); st != "pending" {
					t.Fatalf("job is %s, want pending with retry held", st)
				}
			} else {
				status, outcome := r.waitOutcome("j1")
				if status != c.status || outcome != c.outcome {
					t.Fatalf("job is %s/%s, want %s/%s", status, outcome, c.status, c.outcome)
				}
			}
			if n := r.e.count(r.path, `SELECT COUNT(*) FROM scheduler_job_attempts WHERE job_id = 'j1'`); n != 1 {
				t.Fatalf("attempts = %d, want 1", n)
			}
			// Nothing runs the job again: the marker holds across heartbeats.
			r.e.heartbeats(r.path, 3)
			if n := r.e.count(r.path, `SELECT COUNT(*) FROM scheduler_job_attempts WHERE job_id = 'j1'`); n != 1 {
				t.Fatalf("attempts after three more heartbeats = %d, want 1", n)
			}
			if n := r.e.count(r.path, `SELECT COUNT(*) FROM dispatch_terminals`); n != 1 {
				t.Fatalf("terminal rows = %d, want 1", n)
			}
			if n := r.execs(); n != c.execs {
				t.Fatalf("execve of the executor = %d, want %d", n, c.execs)
			}
			if c.check != nil {
				c.check(r, p, did)
			}
			r.stop(p)
		})
	}
}

// Barrier 5 in fallback mode: the worker has a recorded PID. A live one is
// adopted and finishes ok (the recorded PID is a handle the sweep never needs);
// a dead one goes through the sweep and ends indeterminate with the retry held,
// even under a retry opt-in, because the job had started.
func TestCLIFallbackBarrier5(t *testing.T) {
	t.Run("alive_is_adopted", func(t *testing.T) {
		r := newRecNoCgroup(t)
		release := r.gateJob("j1", nil)
		r.crashAt("after_identity_before_running_cas", "")
		pid := r.dispatchPID()
		if pid <= 1 || procGone(pid) {
			t.Fatalf("recorded pid %d is not a live worker", pid)
		}
		old := r.token("j1")
		p := r.run(nil)
		p.waitReady(t)
		r.waitAdopted("j1", old)
		release()
		r.e.waitJob(r.path, "j1", "completed")
		if status, outcome := r.e.jobOutcome(r.path, "j1"); status != "completed" || outcome != "ok" {
			t.Fatalf("job is %s/%s, want completed/ok", status, outcome)
		}
		if b := r.blocked("j1"); b != "" {
			t.Fatalf("retry_blocked = %q on an adopted worker", b)
		}
		if n := r.execs(); n != 1 {
			t.Fatalf("execve of the executor = %d, want 1", n)
		}
		r.stop(p)
	})
	t.Run("dead_is_indeterminate_and_blocked", func(t *testing.T) {
		r := newRecNoCgroup(t)
		r.gateJob("j1", idempotentRetry)
		r.crashAt("after_identity_before_running_cas", "")
		did := r.dispatchID()
		killCarriers(t, did)
		p := r.run(nil)
		p.waitReady(t)
		if got, want := r.waitBlocked("j1"), "survivor_unknown:"+did; got != want {
			t.Fatalf("retry_blocked = %q, want %q", got, want)
		}
		r.noSecondAttempt(1)
		if n := r.execs(); n != 1 {
			t.Fatalf("execve of the executor = %d, want 1", n)
		}
		r.stop(p)
	})
}

// P3: a candidate whose environment cannot be read is not absent. The job is
// held (retry_blocked) and no attempt runs, even with an indeterminate retry
// opt-in, until the operator confirms with `ic scheduler unblock`; then the
// retry proceeds and the job completes on its second attempt.
func TestCLIFallbackP3UnreadableCandidateBlocksUntilUnblock(t *testing.T) {
	r := newRecNoCgroup(t)
	r.e.addJobWith(r.path, "j1", "ok", idempotentRetry)
	r.crashAt("after_start_before_identity", "")
	did := r.dispatchID()

	// A fixture procfs root with one process whose environ cannot be read.
	root := t.TempDir()
	const fake = 4242
	if err := os.MkdirAll(filepath.Join(root, strconv.Itoa(fake), "environ"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, strconv.Itoa(fake), "stat"), []byte(procStat(fake, 'S', fake, 9001)), 0o644); err != nil {
		t.Fatal(err)
	}
	rootFile := filepath.Join(t.TempDir(), "procroot")
	if err := os.WriteFile(rootFile, []byte(root), 0o600); err != nil {
		t.Fatal(err)
	}
	p := r.run([]string{"IC_SCHED_FAULT=procfs_root_file:" + rootFile})
	p.waitReady(t)
	if got, want := r.waitBlocked("j1"), "survivor_unknown:"+did; got != want {
		t.Fatalf("retry_blocked = %q, want %q", got, want)
	}
	if st := r.e.jobStatus(r.path, "j1"); st != "pending" {
		t.Fatalf("job is %s, want pending (held)", st)
	}
	r.noSecondAttempt(1)
	if n := r.execs(); n != 1 {
		t.Fatalf("execve of the executor = %d, want 1 while blocked", n)
	}
	r.stop(p)

	out, errs, code := r.e.ic("--db=q.db", "scheduler", "unblock", "j1", "--confirm-not-running")
	if code != 0 {
		t.Fatalf("unblock exit %d: %s %s", code, out, errs)
	}
	if b := r.blocked("j1"); b != "" {
		t.Fatalf("retry_blocked = %q after unblock", b)
	}
	p2 := r.run(nil)
	p2.waitReady(t)
	r.e.waitJob(r.path, "j1", "completed")
	if n := r.e.count(r.path, `SELECT COUNT(*) FROM scheduler_job_attempts WHERE job_id = 'j1'`); n != 2 {
		t.Fatalf("attempts = %d, want 2", n)
	}
	if n := r.execs(); n != 2 {
		t.Fatalf("execve of the executor = %d, want 2 after unblock", n)
	}
	r.stop(p2)
}

// procStat builds a /proc/<pid>/stat line with the given state, process group
// and start time (the 20th field after the pid and name).
func procStat(pid int, state byte, pgrp int, start uint64) string {
	f := []string{string(state), "1", strconv.Itoa(pgrp), "0", "0", "0"}
	for len(f) < 19 {
		f = append(f, "0")
	}
	f = append(f, strconv.FormatUint(start, 10))
	for len(f) < 50 {
		f = append(f, "0")
	}
	return fmt.Sprintf("%d (a b) %s\n", pid, strings.Join(f, " "))
}
