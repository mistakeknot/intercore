//go:build icsched_test && linux

package runner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mistakeknot/intercore/internal/scheduler"
)

// Retry tests (plan section 9, R1 to R5). The job's backoff is durable
// (not_before is written by the finalize transaction), so a test moves the
// runner's clock instead of sleeping: it waits for the observable state
// "pending with a not_before", shifts the clock, and waits for the next
// observable state. A shift that must NOT start the retry is checked against a
// claim-pass counter, so the negative control waits for whole passes instead of
// a duration.

// testClock is the runner's clock: real time plus an offset the test moves.
type testClock struct{ off atomic.Int64 }

func (c *testClock) Now() time.Time          { return time.Now().Add(time.Duration(c.off.Load())) }
func (c *testClock) Advance(d time.Duration) { c.off.Add(int64(d)) }

// passes counts claim passes, so a test can wait for whole passes after an
// action instead of sleeping for a guessed time.
type passes struct{ n atomic.Int64 }

func (p *passes) hook(bool) { p.n.Add(1) }

// settle waits for two complete claim passes after the call. The hook fires at
// the start of a pass, so the second increment proves a full pass ran after it.
func (p *passes) settle(t *testing.T) {
	t.Helper()
	want := p.n.Load() + 2
	until(t, "two claim passes", func() bool { return p.n.Load() >= want })
}

func policyJSON(maxAttempts int, baseS, factor, maxS float64, on ...string) string {
	q := make([]string, len(on))
	for i, c := range on {
		q[i] = fmt.Sprintf("%q", c)
	}
	return fmt.Sprintf(`{"max_attempts":%d,"backoff":{"base_s":%g,"factor":%g,"max_s":%g,"jitter":0},"retry_on":[%s]}`,
		maxAttempts, baseS, factor, maxS, strings.Join(q, ","))
}

func withPolicy(p string, idempotent bool) jobOpt {
	return func(j *scheduler.SpawnJob) { j.RetryPolicy = p; j.Idempotent = idempotent }
}

// retrying reports whether the job's attempt n has ended and the job is back
// in pending with a not_before: the durable backoff state.
func (r *rig) retrying(id string, n int) bool {
	j := r.get(id)
	return j.Status == scheduler.StatusPending && r.attempts(id) == n &&
		r.count(`SELECT COUNT(*) FROM scheduler_job_attempts WHERE job_id = ? AND attempt_no = ? AND ended_at IS NOT NULL`, id, n) == 1 &&
		!j.NotBefore.IsZero()
}

func (r *rig) attemptCol(id string, n int, col string) string {
	r.t.Helper()
	var v *string
	q := fmt.Sprintf(`SELECT CAST(%s AS TEXT) FROM scheduler_job_attempts WHERE job_id = ? AND attempt_no = ?`, col)
	if err := r.raw.QueryRow(q, id, n).Scan(&v); err != nil {
		r.t.Fatalf("%s: %v", q, err)
	}
	if v == nil {
		return ""
	}
	return *v
}

func (r *rig) clockOpts(c *testClock) Options {
	o := r.opts()
	o.Now = c.Now
	return o
}

// TestR1PrestartRetriesExhaustWithoutRefund (R1): Start fails with EAGAIN every
// time and max_attempts is 3. The job gets exactly three attempts, each with a
// later not_before than the one before, then fails as prestart. The executor is
// never run, and no failed claim is refunded.
func TestR1PrestartRetriesExhaustWithoutRefund(t *testing.T) {
	r := newRig(t)
	clk := &testClock{}
	r.job("j1", "ok", withPolicy(policyJSON(3, 10, 2, 1000, "prestart"), false))
	o := r.clockOpts(clk)
	o.hooks.startFault = func() error {
		return errors.New("fork/exec: resource temporarily unavailable (injected EAGAIN)")
	}
	run := start(t, o)

	var prev int64
	for n := 1; n <= 2; n++ {
		until(t, fmt.Sprintf("attempt %d to end and back off", n), func() bool { return r.retrying("j1", n) })
		j := r.get("j1")
		if j.AttemptSeq != n {
			t.Fatalf("attempt_seq = %d after attempt %d", j.AttemptSeq, n)
		}
		nb := j.NotBefore.Unix()
		if nb <= prev {
			t.Fatalf("not_before %d after attempt %d is not later than %d", nb, n, prev)
		}
		// The delay is base_s * factor^(n-1), with no jitter: 10 s, then 20 s.
		ended := int64(r.count(`SELECT ended_at FROM scheduler_job_attempts WHERE job_id = 'j1' AND attempt_no = ?`, n))
		if d := nb - ended; d < int64(10<<(n-1))-1 || d > int64(10<<(n-1))+1 {
			t.Fatalf("attempt %d: not_before is %d s after it ended, want %d", n, d, 10<<(n-1))
		}
		prev = nb
		if got := r.attemptCol("j1", n, "outcome_class"); got != scheduler.ClassPrestart {
			t.Fatalf("attempt %d outcome %q", n, got)
		}
		clk.Advance(time.Hour)
	}
	r.waitStatus("j1", scheduler.StatusFailed)
	run.stop(t)

	if got := r.outcome("j1"); got != scheduler.ClassPrestart {
		t.Fatalf("outcome %q", got)
	}
	if j := r.get("j1"); j.AttemptSeq != 3 || j.CrashRequeues != 0 {
		t.Fatalf("attempt_seq=%d crash_requeues=%d, want 3 and 0 (no refund)", j.AttemptSeq, j.CrashRequeues)
	}
	if n := r.attempts("j1"); n != 3 {
		t.Fatalf("attempt rows = %d", n)
	}
	if n := r.runs(); n != 0 {
		t.Fatalf("executor ran %d times, want 0", n)
	}
}

// TestR1bMissingExecutorIsConfigErrorNotRetried (R1b): with the dispatch.sh
// path missing the claim-time check fails the job as config_error. No attempt
// row is written and the retry policy, which lists prestart, is never consulted.
func TestR1bMissingExecutorIsConfigErrorNotRetried(t *testing.T) {
	r := newRig(t)
	r.job("j1", "ok", withPolicy(policyJSON(3, 1, 1, 1, "prestart"), false))
	o := r.opts()
	o.DispatchSH = filepath.Join(r.dir, "no-such-dispatch.sh")
	var p passes
	o.hooks.claimPass = p.hook
	run := start(t, o)
	r.waitStatus("j1", scheduler.StatusFailed)
	p.settle(t) // a retry would have been claimed within these passes
	run.stop(t)

	if got := r.outcome("j1"); got != scheduler.ClassConfigError {
		t.Fatalf("outcome %q", got)
	}
	if n := r.attempts("j1"); n != 0 {
		t.Fatalf("attempt rows = %d", n)
	}
	if j := r.get("j1"); j.AttemptSeq != 0 || r.get("j1").Status != scheduler.StatusFailed {
		t.Fatalf("attempt_seq=%d status=%s", j.AttemptSeq, j.Status)
	}
}

// TestR3CapacityBacksOffAtLeastFifteenMinutes (R3): a capacity failure of an
// idempotent job retries no sooner than 15 minutes later, whatever the policy's
// own backoff says. The negative control moves the clock almost 15 minutes and
// shows the job is still held.
func TestR3CapacityBacksOffAtLeastFifteenMinutes(t *testing.T) {
	r, o := containRig(t) // a started attempt retries only when its cgroup proved it empty
	clk := &testClock{}
	r.job("j1", "receipt quota_exhausted", withPolicy(policyJSON(2, 1, 1, 1, "capacity"), true))
	o.Now = clk.Now
	var p passes
	o.hooks.claimPass = p.hook
	run := start(t, o)

	until(t, "attempt 1 to back off", func() bool { return r.retrying("j1", 1) })
	if got := r.attemptCol("j1", 1, "outcome_class"); got != scheduler.ClassCapacity {
		t.Fatalf("attempt 1 outcome %q", got)
	}
	ended := int64(r.count(`SELECT ended_at FROM scheduler_job_attempts WHERE job_id = 'j1' AND attempt_no = 1`))
	if d := r.get("j1").NotBefore.Unix() - ended; d < 15*60 {
		t.Fatalf("not_before is only %d s after the capacity failure", d)
	}

	clk.Advance(14 * time.Minute) // control: not yet
	p.settle(t)
	if n := r.attempts("j1"); n != 1 || r.runs() != 1 {
		t.Fatalf("retried early: attempts=%d runs=%d", n, r.runs())
	}

	clk.Advance(2 * time.Minute) // 16 minutes in
	r.waitStatus("j1", scheduler.StatusFailed)
	run.stop(t)
	if n := r.attempts("j1"); n != 2 || r.runs() != 2 {
		t.Fatalf("attempts=%d runs=%d, want 2 and 2", n, r.runs())
	}
	if got := r.outcome("j1"); got != scheduler.ClassCapacity {
		t.Fatalf("outcome %q", got)
	}
}

// TestR4RestartDuringBackoffFiresRetryOnce (R4): the runner is stopped while a
// job waits out its backoff. A new runner does not claim it early and, once the
// clock passes not_before, claims it exactly once.
func TestR4RestartDuringBackoffFiresRetryOnce(t *testing.T) {
	r := newRig(t)
	clk := &testClock{}
	r.job("j1", "ok", withPolicy(policyJSON(2, 30, 1, 30, "prestart"), false))

	o1 := r.clockOpts(clk)
	var failed atomic.Bool
	o1.hooks.startFault = func() error {
		if failed.CompareAndSwap(false, true) {
			return errors.New("fork/exec: resource temporarily unavailable (injected EAGAIN)")
		}
		return nil
	}
	run1 := start(t, o1)
	until(t, "attempt 1 to back off", func() bool { return r.retrying("j1", 1) })
	run1.stop(t)
	nb := r.get("j1").NotBefore

	o2 := r.clockOpts(clk)
	var p passes
	o2.hooks.claimPass = p.hook
	run2 := start(t, o2)
	if run2.epoch <= run1.epoch {
		t.Fatalf("epochs %d then %d", run1.epoch, run2.epoch)
	}
	p.settle(t) // control: before not_before, the new runner leaves it alone
	if n := r.attempts("j1"); n != 1 || r.runs() != 0 {
		t.Fatalf("claimed before not_before: attempts=%d runs=%d", n, r.runs())
	}
	if got := r.get("j1").NotBefore; !got.Equal(nb) {
		t.Fatalf("not_before changed across the restart: %v then %v", nb, got)
	}

	clk.Advance(31 * time.Second)
	r.waitStatus("j1", scheduler.StatusCompleted)
	p.settle(t) // a double fire would have shown by now
	run2.stop(t)
	if n := r.attempts("j1"); n != 2 {
		t.Fatalf("attempt rows = %d", n)
	}
	if n := r.runs(); n != 1 {
		t.Fatalf("executor runs = %d, want 1", n)
	}
	if got := r.outcome("j1"); got != scheduler.ClassOK {
		t.Fatalf("outcome %q", got)
	}
}

// TestR5AttemptOutputsAreIsolated (R5): attempt 1 writes a failing verdict and
// a receipt; attempt 2 exits without a verdict. Attempt 2 is indeterminate: it
// never reads attempt 1's verdict, because each attempt has its own output path
// and the runner reads only the path stored on the attempt row.
func TestR5AttemptOutputsAreIsolated(t *testing.T) {
	r, o := containRig(t) // a started attempt retries only when its cgroup proved it empty
	clk := &testClock{}
	r.job("j1", "fail-then-noverdict tool_error",
		withPolicy(policyJSON(2, 1, 1, 1, "agent_failed", "indeterminate"), true))
	o.Now = clk.Now
	run := start(t, o)

	until(t, "attempt 1 to back off", func() bool { return r.retrying("j1", 1) })
	if got := r.attemptCol("j1", 1, "outcome_class"); got != scheduler.ClassAgentFailed {
		t.Fatalf("attempt 1 outcome %q", got)
	}
	clk.Advance(time.Hour)
	r.waitStatus("j1", scheduler.StatusFailed)
	run.stop(t)

	if got := r.attemptCol("j1", 2, "outcome_class"); got != scheduler.ClassIndeterminate {
		t.Fatalf("attempt 2 outcome %q, want indeterminate", got)
	}
	if got := r.outcome("j1"); got != scheduler.ClassIndeterminate {
		t.Fatalf("job outcome %q", got)
	}
	base := filepath.Join(r.dir, "out-j1")
	for n := 1; n <= 2; n++ {
		want := fmt.Sprintf("%s.jj1.a%d", base, n)
		if got := r.attemptCol("j1", n, "output_file"); got != want {
			t.Fatalf("attempt %d output_file %q, want %q", n, got, want)
		}
	}
	// Attempt 1's evidence is on disk, so a runner that shared one path would
	// have found a verdict for attempt 2; attempt 2's own path has none.
	if _, err := os.Stat(base + ".jj1.a1.verdict"); err != nil {
		t.Fatalf("attempt 1 verdict: %v", err)
	}
	if _, err := os.Stat(base + ".jj1.a2.verdict"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("attempt 2 verdict exists: %v", err)
	}
	if v := r.count(`SELECT COUNT(*) FROM dispatches d JOIN scheduler_job_attempts a ON a.dispatch_id = d.id
		WHERE a.job_id = 'j1' AND a.attempt_no = 2 AND COALESCE(d.verdict_status, '') <> ''`); v != 0 {
		t.Fatalf("attempt 2's dispatch carries a verdict")
	}
	if n := r.dispatches("j1"); n != 2 {
		t.Fatalf("dispatches = %d, want a fresh dispatch per attempt", n)
	}
}
