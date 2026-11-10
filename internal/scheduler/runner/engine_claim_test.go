//go:build icsched_test && linux

package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mistakeknot/intercore/internal/scheduler"
)

// passCounter counts the runner's claim passes by the pause flag each one read.
// A test that asserts a job was NOT claimed first waits for fresh passes, so
// the absence is observed after the loop demonstrably ran, never after a sleep.
type passCounter struct{ paused, open atomic.Int64 }

func (p *passCounter) hook(paused bool) {
	if paused {
		p.paused.Add(1)
	} else {
		p.open.Add(1)
	}
}

// waitPasses blocks until n more passes than now have run.
func waitPasses(t *testing.T, c *atomic.Int64, n int64) {
	t.Helper()
	want := c.Load() + n
	until(t, "claim passes to run", func() bool { return c.Load() >= want })
}

func gateFile(r *rig, id string) string { return filepath.Join(r.dir, id+".gate") }

func (r *rig) gateJob(id string, mut ...jobOpt) string {
	r.t.Helper()
	return r.job(id, "gate "+gateFile(r, id), mut...)
}

func (r *rig) mustExec(q string, args ...any) {
	r.t.Helper()
	if _, err := r.raw.Exec(q, args...); err != nil {
		r.t.Fatalf("%s: %v", q, err)
	}
}

// Unit 3: a job that is not due, or is held by retry_blocked, is never
// claimed; a control job in the same queue proves the loop was claiming.
func TestClaimHonoursNotBeforeAndRetryBlocked(t *testing.T) {
	r := newRig(t)
	r.job("future", "ok", func(j *scheduler.SpawnJob) { j.NotBefore = time.Unix(4_000_000_000, 0) })
	r.job("blocked", "ok")
	r.mustExec(`UPDATE scheduler_jobs SET retry_blocked = 'survivor_unknown:x' WHERE id = 'blocked'`)
	r.job("ctrl", "ok")

	var pc passCounter
	o := r.opts()
	o.hooks.claimPass = pc.hook
	run := start(t, o)
	r.waitStatus("ctrl", scheduler.StatusCompleted)
	waitPasses(t, &pc.open, 5) // passes that ran after the control was claimed

	for _, id := range []string{"future", "blocked"} {
		if st := r.get(id).Status; st != scheduler.StatusPending {
			t.Fatalf("%s is %s, want pending", id, st)
		}
		if n := r.attempts(id); n != 0 {
			t.Fatalf("%s has %d attempts", id, n)
		}
	}
	if n := r.runs(); n != 1 {
		t.Fatalf("executor runs = %d, want only the control's", n)
	}

	// Negative control: lifting each gate lets the same row run.
	r.mustExec(`UPDATE scheduler_jobs SET not_before = 0 WHERE id = 'future'`)
	r.mustExec(`UPDATE scheduler_jobs SET retry_blocked = NULL WHERE id = 'blocked'`)
	r.waitStatus("future", scheduler.StatusCompleted)
	r.waitStatus("blocked", scheduler.StatusCompleted)
	run.stop(t)
	if n := r.runs(); n != 3 {
		t.Fatalf("executor runs = %d, want 3", n)
	}
}

// Unit 3: a pause that commits between candidate selection and the claim
// stops that claim: no attempt row, no dispatch, the job stays pending.
func TestPauseCommittedBetweenSelectAndClaim(t *testing.T) {
	r := newRig(t)
	r.job("j", "ok")

	var once sync.Once
	var selected atomic.Value
	var hookErr atomic.Value
	var pc passCounter
	o := r.opts()
	o.hooks.claimPass = pc.hook
	o.hooks.afterCandidates = func(ids []string) {
		once.Do(func() {
			selected.Store(append([]string(nil), ids...))
			if err := writePause(r.raw, true); err != nil {
				hookErr.Store(err)
			}
		})
	}
	run := start(t, o)
	until(t, "the candidate hook to fire", func() bool { return selected.Load() != nil })
	if err, _ := hookErr.Load().(error); err != nil {
		t.Fatal(err)
	}
	if got := selected.Load().([]string); !reflect.DeepEqual(got, []string{"j"}) {
		t.Fatalf("candidates = %v", got)
	}
	waitPasses(t, &pc.paused, 5)
	if st := r.get("j").Status; st != scheduler.StatusPending {
		t.Fatalf("job is %s, want pending after the pause won the race", st)
	}
	if n := r.attempts("j"); n != 0 {
		t.Fatalf("attempts = %d", n)
	}
	if n := r.runs(); n != 0 {
		t.Fatalf("executor runs = %d", n)
	}

	// Negative control: without the pause the same job runs once.
	setPaused(t, r.raw, false)
	r.waitStatus("j", scheduler.StatusCompleted)
	run.stop(t)
	if r.attempts("j") != 1 || r.runs() != 1 {
		t.Fatalf("attempts=%d runs=%d", r.attempts("j"), r.runs())
	}
}

// Unit 3: never more than max_concurrent jobs in flight; a freed slot is used.
func TestMaxConcurrentIsRespected(t *testing.T) {
	r := newRig(t)
	prompts := map[string]string{}
	for _, id := range []string{"g1", "g2", "g3"} {
		prompts[id] = r.gateJob(id)
	}
	var peak atomic.Int64
	var pc passCounter
	o := r.opts()
	o.MaxConcurrent = 2
	o.hooks.claimPass = func(paused bool) {
		pc.hook(paused)
		var n int64
		_ = r.raw.QueryRow(`SELECT COUNT(*) FROM scheduler_jobs WHERE status IN ('launching','running','cancelling')`).Scan(&n)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
	}
	run := start(t, o)
	r.waitStatus("g1", scheduler.StatusRunning)
	r.waitStatus("g2", scheduler.StatusRunning)
	waitPasses(t, &pc.open, 5)
	if st := r.get("g3").Status; st != scheduler.StatusPending {
		t.Fatalf("g3 is %s, want pending while both slots are busy", st)
	}
	if n := r.attempts("g3"); n != 0 {
		t.Fatalf("g3 attempts = %d", n)
	}

	// Negative control: a freed slot is claimed.
	release(t, prompts["g1"])
	r.waitStatus("g1", scheduler.StatusCompleted)
	r.waitStatus("g3", scheduler.StatusRunning)
	release(t, prompts["g2"])
	release(t, prompts["g3"])
	r.waitStatus("g2", scheduler.StatusCompleted)
	r.waitStatus("g3", scheduler.StatusCompleted)
	run.stop(t)
	if p := peak.Load(); p > 2 || p < 2 {
		t.Fatalf("peak in flight = %d, want exactly 2", p)
	}
}

// Unit 12: urgent before low regardless of arrival order; equal priorities in
// creation order, not id order. One slot, so the log order is the run order.
func TestClaimOrderIsPriorityThenAge(t *testing.T) {
	r := newRig(t)
	at := func(n int64) jobOpt {
		return func(j *scheduler.SpawnJob) { j.CreatedAt = time.Unix(1_800_000_000+n, 0) }
	}
	pri := func(p scheduler.JobPriority) jobOpt { return func(j *scheduler.SpawnJob) { j.Priority = p } }
	created := []string{"low", "zz-normal-old", "aa-normal-new", "high", "urgent"}
	r.job("low", "ok", pri(scheduler.PriorityLow), at(0))
	r.job("zz-normal-old", "ok", pri(scheduler.PriorityNormal), at(1))
	r.job("aa-normal-new", "ok", pri(scheduler.PriorityNormal), at(2))
	r.job("high", "ok", pri(scheduler.PriorityHigh), at(3))
	r.job("urgent", "ok", pri(scheduler.PriorityUrgent), at(4))

	o := r.opts()
	o.MaxConcurrent = 1
	run := start(t, o)
	for _, id := range created {
		r.waitStatus(id, scheduler.StatusCompleted)
	}
	run.stop(t)

	b, err := os.ReadFile(r.log)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		for _, f := range strings.Fields(line) {
			if v, ok := strings.CutPrefix(f, "job="); ok {
				got = append(got, v)
			}
		}
	}
	want := []string{"urgent", "high", "zz-normal-old", "aa-normal-new", "low"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("run order = %v, want %v", got, want)
	}
	if reflect.DeepEqual(got, created) {
		t.Fatal("run order equals creation order: the test proves nothing")
	}
}

// Unit 13: every claim-time failure is a config_error naming its reason, with
// no attempt row, no dispatch row and no executor run. A valid job beside it
// is claimed, so the failure is the check and not a dead loop.
func TestClaimTimeChecksFailAsConfigError(t *testing.T) {
	prompt := func(r *rig, id string) string { return filepath.Join(r.dir, id+".prompt") }
	cases := []struct {
		name   string
		reason string
		setup  func(r *rig, o *Options)
	}{
		{"missing executor", reasonNoExecutor, func(r *rig, o *Options) {
			o.DispatchSH = filepath.Join(r.dir, "no-such-dispatch.sh")
			r.job("bad", "ok")
		}},
		{"non executable executor", reasonNoExecutor, func(r *rig, o *Options) {
			p := filepath.Join(r.dir, "plain.sh")
			if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			o.DispatchSH = p
			r.job("bad", "ok")
		}},
		{"missing project dir", reasonNoProjectDir, func(r *rig, o *Options) {
			r.job("bad", "ok", func(j *scheduler.SpawnJob) { j.ProjectDir = filepath.Join(r.dir, "gone") })
		}},
		{"missing prompt", reasonMissingPrmpt, func(r *rig, o *Options) {
			r.job("bad", "ok")
			if err := os.Remove(prompt(r, "bad")); err != nil {
				t.Fatal(err)
			}
		}},
		{"existing output path", reasonOutputExists, func(r *rig, o *Options) {
			r.job("bad", "ok")
			if err := os.WriteFile(outputFor(filepath.Join(r.dir, "out-bad"), "bad", 1), []byte("old"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"existing sidecar", reasonOutputExists, func(r *rig, o *Options) {
			r.job("bad", "ok")
			if err := os.WriteFile(outputFor(filepath.Join(r.dir, "out-bad"), "bad", 1)+".verdict", []byte("old"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"governed role", reasonGoverned, func(r *rig, o *Options) {
			so, _ := json.Marshal(map[string]any{"AgentType": "codex", "PromptFile": prompt(r, "bad"), "Role": "cross-lab-review"})
			r.job("bad", "ok", func(j *scheduler.SpawnJob) { j.SpawnOpts = string(so) })
		}},
		{"governed decision", reasonGoverned, func(r *rig, o *Options) {
			so, _ := json.Marshal(map[string]any{"AgentType": "codex", "PromptFile": prompt(r, "bad"), "Decision": map[string]any{"RequestedRole": "x"}})
			r.job("bad", "ok", func(j *scheduler.SpawnJob) { j.SpawnOpts = string(so) })
		}},
		{"governed null role key", reasonGoverned, func(r *rig, o *Options) {
			so := `{"AgentType":"codex","PromptFile":"` + prompt(r, "bad") + `","role":null}`
			r.job("bad", "ok", func(j *scheduler.SpawnJob) { j.SpawnOpts = so })
		}},
		{"governed beats malformed fields", reasonGoverned, func(r *rig, o *Options) {
			so := `{"Role":"validation","TimeoutSec":"invalid"}`
			r.job("bad", "ok", func(j *scheduler.SpawnJob) { j.SpawnOpts = so })
		}},
		{"run and scope ids disagree", reasonBadSpawnOpts, func(r *rig, o *Options) {
			so, _ := json.Marshal(map[string]any{"AgentType": "codex", "PromptFile": prompt(r, "bad"), "RunID": "a", "ScopeID": "b"})
			r.job("bad", "ok", func(j *scheduler.SpawnJob) { j.SpawnOpts = string(so) })
		}},
		{"unparseable spawn opts", reasonBadSpawnOpts, func(r *rig, o *Options) {
			r.job("bad", "ok", func(j *scheduler.SpawnJob) { j.SpawnOpts = "{not json" })
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			o := r.opts()
			tc.setup(r, &o)
			r.job("ctrl", "ok")
			run := start(t, o)
			r.waitStatus("bad", scheduler.StatusFailed)
			run.stop(t)

			if got := r.outcome("bad"); got != scheduler.ClassConfigError {
				t.Fatalf("outcome = %q", got)
			}
			if msg := r.get("bad").Error; !strings.HasPrefix(msg, tc.reason+":") {
				t.Fatalf("error %q does not start with %q", msg, tc.reason)
			}
			if n := r.attempts("bad"); n != 0 {
				t.Fatalf("attempt rows = %d", n)
			}
			if n := r.count(`SELECT COUNT(*) FROM dispatches WHERE prompt_file = ?`, prompt(r, "bad")); n != 0 {
				t.Fatalf("dispatch rows for the failed job = %d", n)
			}
			for _, e := range readLog(t, r) {
				if strings.Contains(e, "job=bad ") || strings.HasSuffix(e, "job=bad") {
					t.Fatalf("the executor ran the failed job: %s", e)
				}
			}
			if tc.reason == reasonNoExecutor {
				// Nothing can run without an executor; the control fails the same way.
				if got := r.outcome("ctrl"); got != scheduler.ClassConfigError {
					t.Fatalf("control outcome = %q", got)
				}
				return
			}
			if r.get("ctrl").Status != scheduler.StatusCompleted || r.runs() != 1 {
				t.Fatalf("control: status=%s runs=%d", r.get("ctrl").Status, r.runs())
			}
		})
	}
}

func readLog(t *testing.T, r *rig) []string {
	t.Helper()
	b, err := os.ReadFile(r.log)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// I5: pause with one job running and two pending. The running job completes,
// the pending jobs wait, and resume runs them.
func TestPauseLetsRunningFinishAndHoldsPending(t *testing.T) {
	r := newRig(t)
	g := r.gateJob("g")
	r.job("p1", "ok", func(j *scheduler.SpawnJob) { j.CreatedAt = time.Unix(1_800_000_001, 0) })
	r.job("p2", "ok", func(j *scheduler.SpawnJob) { j.CreatedAt = time.Unix(1_800_000_002, 0) })
	var pc passCounter
	o := r.opts()
	o.MaxConcurrent = 1
	o.hooks.claimPass = pc.hook
	run := start(t, o)
	r.waitStatus("g", scheduler.StatusRunning)

	setPaused(t, r.raw, true)
	release(t, g)
	r.waitStatus("g", scheduler.StatusCompleted)
	waitPasses(t, &pc.paused, 5) // passes after the running job finished
	for _, id := range []string{"p1", "p2"} {
		if st := r.get(id).Status; st != scheduler.StatusPending {
			t.Fatalf("%s is %s while paused", id, st)
		}
		if n := r.attempts(id); n != 0 {
			t.Fatalf("%s attempts = %d while paused", id, n)
		}
	}
	if n := r.runs(); n != 1 {
		t.Fatalf("executor runs = %d while paused", n)
	}

	// Negative control: resume runs both.
	setPaused(t, r.raw, false)
	r.waitStatus("p1", scheduler.StatusCompleted)
	r.waitStatus("p2", scheduler.StatusCompleted)
	run.stop(t)
	if n := r.runs(); n != 3 {
		t.Fatalf("executor runs = %d after resume", n)
	}
}

// I6: a pause survives a runner restart: the new runner claims nothing until
// the queue is resumed.
func TestPauseSurvivesRunnerRestart(t *testing.T) {
	r := newRig(t)
	r.job("j", "ok")
	setPaused(t, r.raw, true)

	var pc1 passCounter
	o := r.opts()
	o.hooks.claimPass = pc1.hook
	first := start(t, o)
	waitPasses(t, &pc1.paused, 3)
	first.stop(t)

	var pc2 passCounter
	o2 := r.opts()
	o2.hooks.claimPass = pc2.hook
	second := start(t, o2)
	if second.epoch <= first.epoch {
		t.Fatalf("epochs %d then %d: the restart did not bump the epoch", first.epoch, second.epoch)
	}
	waitPasses(t, &pc2.paused, 5)
	if st := r.get("j").Status; st != scheduler.StatusPending {
		t.Fatalf("job is %s after the restart, want pending", st)
	}
	if r.attempts("j") != 0 || r.runs() != 0 {
		t.Fatalf("attempts=%d runs=%d while paused", r.attempts("j"), r.runs())
	}

	// Negative control: resume runs it on the restarted runner.
	setPaused(t, r.raw, false)
	r.waitStatus("j", scheduler.StatusCompleted)
	second.stop(t)
	if r.runs() != 1 {
		t.Fatalf("runs = %d", r.runs())
	}
}

// A plain job queued by an older client carries `"Decision":null`; it is not
// governed and must run. This is the control for the governed cases above: the
// governed check keys on a non-null decision, not on the key's presence.
func TestNullDecisionPlainJobRuns(t *testing.T) {
	r := newRig(t)
	r.job("plain", "ok", func(j *scheduler.SpawnJob) {
		so := `{"Decision":null,"AgentType":"codex","PromptFile":"` + filepath.Join(r.dir, "plain.prompt") + `"}`
		j.SpawnOpts = so
	})
	run := start(t, r.opts())
	r.waitStatus("plain", scheduler.StatusCompleted)
	run.stop(t)
	if n := r.runs(); n != 1 {
		t.Fatalf("executor runs = %d", n)
	}
}
