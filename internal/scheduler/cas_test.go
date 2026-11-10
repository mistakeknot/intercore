package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

// fx is a migrated throwaway DB plus a store. Every test that touches the
// scheduler tables builds its own, so no test shares a runner epoch.
type fx struct {
	t   *testing.T
	db  *sql.DB
	s   *Store
	ctx context.Context
	now time.Time
}

func newFx(t *testing.T) *fx {
	t.Helper()
	db := openTestDB(t)
	return &fx{t: t, db: db, s: NewStore(db), ctx: context.Background(), now: time.Unix(1_800_000_000, 0)}
}

func (f *fx) exec(q string, args ...any) {
	f.t.Helper()
	if _, err := f.db.Exec(q, args...); err != nil {
		f.t.Fatalf("%s: %v", q, err)
	}
}

func (f *fx) job(id string, mut func(*SpawnJob)) {
	f.t.Helper()
	j := &SpawnJob{
		ID: id, Status: StatusPending, Priority: PriorityNormal, AgentType: "codex",
		SpawnOpts: "{}", MaxRetries: 3, CreatedAt: f.now,
	}
	if mut != nil {
		mut(j)
	}
	if err := f.s.Create(f.ctx, j); err != nil {
		f.t.Fatalf("create %s: %v", id, err)
	}
}

func (f *fx) dispatch(id string) {
	f.t.Helper()
	f.exec(`INSERT INTO dispatches (id, project_dir, status) VALUES (?, '/tmp/p', 'running')`, id)
}

func (f *fx) setEpoch(e int64) {
	f.t.Helper()
	f.exec(`UPDATE scheduler_runner SET epoch = ? WHERE id = 1`, e)
}

func (f *fx) status(id string) JobStatus {
	f.t.Helper()
	j, err := f.s.Get(f.ctx, id)
	if err != nil {
		f.t.Fatalf("get %s: %v", id, err)
	}
	return j.Status
}

func (f *fx) claim(id string, epoch int64) *Claim {
	f.t.Helper()
	c, err := f.s.Claim(f.ctx, ClaimParams{JobID: id, Epoch: epoch, Now: f.now})
	if err != nil {
		f.t.Fatalf("claim %s: %v", id, err)
	}
	return c
}

func (f *fx) running(id string, epoch int64) *Claim {
	f.t.Helper()
	c := f.claim(id, epoch)
	f.dispatch("d-" + id)
	if err := f.s.Link(f.ctx, c, "d-"+id, f.now); err != nil {
		f.t.Fatalf("link: %v", err)
	}
	if err := f.s.MarkRunning(f.ctx, c, f.now); err != nil {
		f.t.Fatalf("run: %v", err)
	}
	return c
}

func (f *fx) attemptInt(id string, no int, col string) sql.NullInt64 {
	f.t.Helper()
	var v sql.NullInt64
	if err := f.db.QueryRow(`SELECT `+col+` FROM scheduler_job_attempts WHERE job_id=? AND attempt_no=?`, id, no).Scan(&v); err != nil {
		f.t.Fatalf("attempt %s/%d %s: %v", id, no, col, err)
	}
	return v
}

func (f *fx) attemptStr(id string, no int, col string) sql.NullString {
	f.t.Helper()
	var v sql.NullString
	if err := f.db.QueryRow(`SELECT `+col+` FROM scheduler_job_attempts WHERE job_id=? AND attempt_no=?`, id, no).Scan(&v); err != nil {
		f.t.Fatalf("attempt %s/%d %s: %v", id, no, col, err)
	}
	return v
}

func (f *fx) get(id string) *SpawnJob {
	f.t.Helper()
	j, err := f.s.Get(f.ctx, id)
	if err != nil {
		f.t.Fatalf("get %s: %v", id, err)
	}
	return j
}

// --- Create/Get round trip of the new columns -------------------------------

func TestStoreRoundTripsRunnerColumns(t *testing.T) {
	f := newFx(t)
	f.job("rt", func(j *SpawnJob) {
		j.RetryPolicy = `{"max_attempts":2}`
		j.Idempotent = true
		j.IdempotencyKey = "k1"
		j.OutputBase = "/tmp/out"
		j.ProjectDir = "/tmp/p"
		j.NotBefore = f.now.Add(time.Minute)
	})
	got := f.get("rt")
	if got.RetryPolicy != `{"max_attempts":2}` || !got.Idempotent || got.IdempotencyKey != "k1" ||
		got.OutputBase != "/tmp/out" || got.ProjectDir != "/tmp/p" || !got.NotBefore.Equal(f.now.Add(time.Minute)) {
		t.Fatalf("round trip lost a column: %+v", got)
	}
	if got.AttemptSeq != 0 || got.CrashRequeues != 0 || got.ClaimToken != "" || got.LeaseEpoch != nil {
		t.Fatalf("fresh job has runner state: %+v", got)
	}
	if got.UpdatedAt.IsZero() {
		t.Fatal("updated_at not set on create")
	}
	// Two jobs may not share an idempotency key.
	dup := &SpawnJob{ID: "rt2", Status: StatusPending, AgentType: "codex", SpawnOpts: "{}", CreatedAt: f.now, IdempotencyKey: "k1"}
	if err := f.s.Create(f.ctx, dup); err == nil {
		t.Fatal("duplicate idempotency key accepted")
	}
}

// --- Claim ------------------------------------------------------------------

func TestClaimMovesPendingToLaunching(t *testing.T) {
	f := newFx(t)
	f.job("c1", nil)
	c := f.claim("c1", 1)
	if c.Token == "" || c.AttemptNo != 1 || c.Epoch != 1 {
		t.Fatalf("claim = %+v", c)
	}
	j := f.get("c1")
	if j.Status != StatusLaunching || j.AttemptSeq != 1 || j.ClaimToken != c.Token || j.LeaseEpoch == nil || *j.LeaseEpoch != 1 {
		t.Fatalf("job after claim = %+v", j)
	}
	if v := f.attemptInt("c1", 1, "counts_toward_limit"); !v.Valid || v.Int64 != 1 {
		t.Fatalf("counts_toward_limit = %v", v)
	}
	if v := f.attemptInt("c1", 1, "claimed_at"); v.Int64 != f.now.Unix() {
		t.Fatalf("claimed_at = %v", v)
	}
	if _, err := f.s.Claim(f.ctx, ClaimParams{JobID: "c1", Epoch: 1, Now: f.now}); !errors.Is(err, ErrLostClaim) {
		t.Fatalf("second claim err = %v, want ErrLostClaim", err)
	}
}

func TestClaimRefusals(t *testing.T) {
	cases := []struct {
		name  string
		setup func(f *fx)
		epoch int64
		now   func(f *fx) time.Time
	}{
		{"stale epoch", func(f *fx) {}, 2, nil},
		{"retry blocked", func(f *fx) { f.exec(`UPDATE scheduler_jobs SET retry_blocked='survivor_unknown:d' WHERE id='x'`) }, 1, nil},
		{"not before in the future", func(f *fx) {
			f.exec(`UPDATE scheduler_jobs SET not_before=? WHERE id='x'`, f.now.Add(time.Hour).Unix())
		}, 1, nil},
		{"paused", func(f *fx) {
			f.exec(`INSERT OR REPLACE INTO state (key, scope_id, payload) VALUES ('scheduler_paused','global','true')`)
		}, 1, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFx(t)
			f.job("x", nil)
			tc.setup(f)
			_, err := f.s.Claim(f.ctx, ClaimParams{JobID: "x", Epoch: tc.epoch, Now: f.now})
			if !errors.Is(err, ErrLostClaim) {
				t.Fatalf("err = %v, want ErrLostClaim", err)
			}
			if st := f.status("x"); st != StatusPending {
				t.Fatalf("status = %s, want pending", st)
			}
			var n int
			f.db.QueryRow(`SELECT count(*) FROM scheduler_job_attempts`).Scan(&n)
			if n != 0 {
				t.Fatalf("a refused claim created %d attempt rows", n)
			}
		})
	}
}

// A claim that is not paused and not blocked is allowed once resume has run.
func TestClaimAllowedWhenResumed(t *testing.T) {
	f := newFx(t)
	f.job("x", nil)
	f.exec(`INSERT OR REPLACE INTO state (key, scope_id, payload) VALUES ('scheduler_paused','global','false')`)
	f.claim("x", 1)
}

// --- Link, marker, evidence -------------------------------------------------

func TestLinkSetsJobAndAttempt(t *testing.T) {
	f := newFx(t)
	f.job("l", nil)
	c := f.claim("l", 1)
	f.dispatch("d1")
	if err := f.s.Link(f.ctx, c, "d1", f.now); err != nil {
		t.Fatal(err)
	}
	if j := f.get("l"); j.DispatchID != "d1" || j.Status != StatusLaunching {
		t.Fatalf("job = %+v", j)
	}
	if v := f.attemptStr("l", 1, "dispatch_id"); v.String != "d1" {
		t.Fatalf("attempt dispatch_id = %v", v)
	}
	if v := f.attemptInt("l", 1, "linked_at"); v.Int64 != f.now.Unix() {
		t.Fatalf("linked_at = %v", v)
	}
	f.dispatch("d2")
	if err := f.s.Link(f.ctx, c, "d2", f.now); !errors.Is(err, ErrLostClaim) {
		t.Fatalf("second link err = %v, want ErrLostClaim", err)
	}
	bad := *c
	bad.Token = "other"
	f.job("l2", nil)
	c2 := f.claim("l2", 1)
	c2.Token = "forged"
	if err := f.s.Link(f.ctx, c2, "d2", f.now); !errors.Is(err, ErrLostClaim) {
		t.Fatalf("forged token err = %v, want ErrLostClaim", err)
	}
}

// The link runs inside the caller's admission transaction; rolling that
// transaction back leaves no link.
func TestLinkTxRollsBackWithAdmission(t *testing.T) {
	f := newFx(t)
	f.job("l", nil)
	c := f.claim("l", 1)
	f.dispatch("d1")
	tx, err := f.db.BeginTx(f.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := LinkTx(f.ctx, tx, c, "d1", f.now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if j := f.get("l"); j.DispatchID != "" {
		t.Fatalf("link survived rollback: %+v", j)
	}
	if v := f.attemptInt("l", 1, "linked_at"); v.Valid {
		t.Fatal("linked_at survived rollback")
	}
}

func TestLinkAbortsWhenCancelling(t *testing.T) {
	f := newFx(t)
	f.job("l", nil)
	c := f.claim("l", 1)
	if _, err := f.s.CancelJob(f.ctx, "l", f.now); err != nil {
		t.Fatal(err)
	}
	f.dispatch("d1")
	if err := f.s.Link(f.ctx, c, "d1", f.now); !errors.Is(err, ErrCancelled) {
		t.Fatalf("err = %v, want ErrCancelled", err)
	}
	if j := f.get("l"); j.DispatchID != "" || j.Status != StatusCancelling {
		t.Fatalf("job = %+v", j)
	}
}

func TestMarkRunningNeedsLink(t *testing.T) {
	f := newFx(t)
	f.job("r", nil)
	c := f.claim("r", 1)
	if err := f.s.MarkRunning(f.ctx, c, f.now); !errors.Is(err, ErrLostClaim) {
		t.Fatalf("unlinked MarkRunning err = %v, want ErrLostClaim (running implies linked)", err)
	}
	if st := f.status("r"); st != StatusLaunching {
		t.Fatalf("status = %s", st)
	}
}

func TestLaunchResultWrittenOnceAndValidated(t *testing.T) {
	f := newFx(t)
	f.job("r", nil)
	c := f.claim("r", 1)
	if _, err := f.s.RecordLaunchResult(f.ctx, c, "bogus"); err == nil {
		t.Fatal("invalid launch_result accepted")
	}
	ok, err := f.s.RecordLaunchResult(f.ctx, c, "rejected")
	if err != nil || !ok {
		t.Fatalf("first write = %v, %v", ok, err)
	}
	ok, err = f.s.RecordLaunchResult(f.ctx, c, "spawn_error")
	if err != nil || ok {
		t.Fatalf("second write = %v, %v, want no change", ok, err)
	}
	if v := f.attemptStr("r", 1, "launch_result"); v.String != "rejected" {
		t.Fatalf("launch_result = %v", v)
	}
}

func TestMarkExitedWrittenOnce(t *testing.T) {
	f := newFx(t)
	f.job("r", nil)
	c := f.running("r", 1)
	ok, err := f.s.MarkExited(f.ctx, c, f.now)
	if err != nil || !ok {
		t.Fatalf("first = %v, %v", ok, err)
	}
	ok, err = f.s.MarkExited(f.ctx, c, f.now.Add(time.Minute))
	if err != nil || ok {
		t.Fatalf("second = %v, %v", ok, err)
	}
	if v := f.attemptInt("r", 1, "exited_at"); v.Int64 != f.now.Unix() {
		t.Fatalf("exited_at = %v", v)
	}
}

// Unit test 14.
func TestEvidenceWriteOnce(t *testing.T) {
	f := newFx(t)
	f.job("e", nil)
	c := f.running("e", 1)
	ok, err := f.s.WriteEvidence(f.ctx, c, "verdict", "aa", "file")
	if err != nil || !ok {
		t.Fatalf("first = %v, %v", ok, err)
	}
	ok, err = f.s.WriteEvidence(f.ctx, c, "none", "bb", "other")
	if err != nil || ok {
		t.Fatalf("second write changed a row: %v, %v", ok, err)
	}
	if v := f.attemptStr("e", 1, "evidence_class"); v.String != "verdict" {
		t.Fatalf("evidence_class = %v", v)
	}
	if v := f.attemptStr("e", 1, "evidence_sha256"); v.String != "aa" {
		t.Fatalf("evidence_sha256 = %v", v)
	}
	if v := f.attemptStr("e", 1, "evidence_source"); v.String != "file" {
		t.Fatalf("evidence_source = %v", v)
	}
	// A stale epoch or token writes nothing.
	f.job("e2", nil)
	c2 := f.running("e2", 1)
	f.setEpoch(2)
	ok, err = f.s.WriteEvidence(f.ctx, c2, "verdict", "aa", "file")
	if err != nil || ok {
		t.Fatalf("stale epoch write = %v, %v", ok, err)
	}
}

// --- Unit 6: every transition in section 4, illegal ones rejected -----------

func TestTransitionTable(t *testing.T) {
	type setup func(f *fx, id string) *Claim
	bogus := &Claim{JobID: "", Token: "bogus", AttemptNo: 1, Epoch: 1}

	states := []struct {
		name  string
		build setup
		want  JobStatus
	}{
		{"pending", func(f *fx, id string) *Claim { return nil }, StatusPending},
		{"launching", func(f *fx, id string) *Claim { return f.claim(id, 1) }, StatusLaunching},
		{"launching-linked", func(f *fx, id string) *Claim {
			c := f.claim(id, 1)
			f.dispatch("d-" + id)
			if err := f.s.Link(f.ctx, c, "d-"+id, f.now); err != nil {
				t.Fatal(err)
			}
			return c
		}, StatusLaunching},
		{"running", func(f *fx, id string) *Claim { return f.running(id, 1) }, StatusRunning},
		{"cancelling", func(f *fx, id string) *Claim {
			c := f.running(id, 1)
			if _, err := f.s.CancelJob(f.ctx, id, f.now); err != nil {
				t.Fatal(err)
			}
			return c
		}, StatusCancelling},
		{"quarantined", func(f *fx, id string) *Claim {
			c := f.claim(id, 1)
			f.exec(`UPDATE scheduler_jobs SET status='quarantined', outcome_class='runner_inconsistent' WHERE id=?`, id)
			return c
		}, StatusQuarantined},
		{"completed", func(f *fx, id string) *Claim {
			c := f.running(id, 1)
			if _, err := f.s.Finalize(f.ctx, c, FinalizeParams{Class: ClassOK, Now: f.now}); err != nil {
				t.Fatal(err)
			}
			return c
		}, StatusCompleted},
		{"failed", func(f *fx, id string) *Claim {
			c := f.running(id, 1)
			if _, err := f.s.Finalize(f.ctx, c, FinalizeParams{Class: ClassAgentFailed, Now: f.now}); err != nil {
				t.Fatal(err)
			}
			return c
		}, StatusFailed},
		{"cancelled", func(f *fx, id string) *Claim {
			c := f.claim(id, 1)
			f.exec(`UPDATE scheduler_jobs SET status='pending', claim_token=NULL, lease_epoch=NULL WHERE id=?`, id)
			if _, err := f.s.CancelJob(f.ctx, id, f.now); err != nil {
				t.Fatal(err)
			}
			return c
		}, StatusCancelled},
	}

	type outcome struct {
		err error
		to  JobStatus // status after the op; "" = unchanged
	}
	ops := []struct {
		name string
		run  func(f *fx, id string, c *Claim) error
	}{
		{"claim", func(f *fx, id string, c *Claim) error {
			_, err := f.s.Claim(f.ctx, ClaimParams{JobID: id, Epoch: 1, Now: f.now.Add(time.Hour)})
			return err
		}},
		{"link", func(f *fx, id string, c *Claim) error {
			f.dispatch("d2-" + id)
			return f.s.Link(f.ctx, claimOrBogus(c, bogus, id), "d2-"+id, f.now)
		}},
		{"run", func(f *fx, id string, c *Claim) error {
			return f.s.MarkRunning(f.ctx, claimOrBogus(c, bogus, id), f.now)
		}},
		{"finalize", func(f *fx, id string, c *Claim) error {
			_, err := f.s.Finalize(f.ctx, claimOrBogus(c, bogus, id), FinalizeParams{Class: ClassOK, Now: f.now})
			return err
		}},
		{"cancel", func(f *fx, id string, c *Claim) error {
			_, err := f.s.CancelJob(f.ctx, id, f.now)
			return err
		}},
		{"requeue", func(f *fx, id string, c *Claim) error {
			return f.s.Requeue(f.ctx, id, true, f.now)
		}},
	}

	L, C, T, Q := ErrLostClaim, ErrCancelled, ErrAlreadyTerminal, ErrNotQuarantined
	// want[state][op]
	want := map[string]map[string]outcome{
		"pending": {
			"claim": {nil, StatusLaunching}, "link": {L, ""}, "run": {L, ""}, "finalize": {L, ""},
			"cancel": {nil, StatusCancelled}, "requeue": {Q, ""},
		},
		"launching": {
			"claim": {L, ""}, "link": {nil, StatusLaunching}, "run": {L, ""}, "finalize": {nil, StatusCompleted},
			"cancel": {nil, StatusCancelling}, "requeue": {Q, ""},
		},
		"launching-linked": {
			"claim": {L, ""}, "link": {L, ""}, "run": {nil, StatusRunning}, "finalize": {nil, StatusCompleted},
			"cancel": {nil, StatusCancelling}, "requeue": {Q, ""},
		},
		"running": {
			"claim": {L, ""}, "link": {L, ""}, "run": {L, ""}, "finalize": {nil, StatusCompleted},
			"cancel": {nil, StatusCancelling}, "requeue": {Q, ""},
		},
		"cancelling": {
			"claim": {L, ""}, "link": {C, ""}, "run": {C, ""}, "finalize": {nil, StatusCancelled},
			"cancel": {nil, ""}, "requeue": {Q, ""},
		},
		"quarantined": {
			"claim": {L, ""}, "link": {L, ""}, "run": {L, ""}, "finalize": {L, ""},
			"cancel": {nil, StatusCancelled}, "requeue": {nil, StatusPending},
		},
	}
	for _, term := range []string{"completed", "failed", "cancelled"} {
		want[term] = map[string]outcome{
			"claim": {L, ""}, "link": {L, ""}, "run": {L, ""}, "finalize": {L, ""},
			"cancel": {T, ""}, "requeue": {Q, ""},
		}
	}

	for _, st := range states {
		for _, op := range ops {
			t.Run(st.name+"/"+op.name, func(t *testing.T) {
				f := newFx(t)
				id := "j"
				f.job(id, nil)
				c := st.build(f, id)
				if got := f.status(id); got != st.want {
					t.Fatalf("setup left status %s, want %s", got, st.want)
				}
				w := want[st.name][op.name]
				err := op.run(f, id, c)
				if w.err == nil && err != nil {
					t.Fatalf("err = %v, want success", err)
				}
				if w.err != nil && !errors.Is(err, w.err) {
					t.Fatalf("err = %v, want %v", err, w.err)
				}
				after := st.want
				if w.to != "" {
					after = w.to
				}
				if got := f.status(id); got != after {
					t.Fatalf("status after = %s, want %s", got, after)
				}
			})
		}
	}
}

func claimOrBogus(c, bogus *Claim, id string) *Claim {
	if c != nil {
		return c
	}
	b := *bogus
	b.JobID = id
	return &b
}

// --- Cancel (5.6) ------------------------------------------------------------

func TestCancelPendingIsDirect(t *testing.T) {
	f := newFx(t)
	f.job("p", nil)
	res, err := f.s.CancelJob(f.ctx, "p", f.now)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusCancelled || res.InFlight {
		t.Fatalf("result = %+v", res)
	}
	j := f.get("p")
	if j.Status != StatusCancelled || j.OutcomeClass != ClassCancelled || j.CompletedAt.IsZero() {
		t.Fatalf("job = %+v", j)
	}
}

// The lost update the CLI used to have: a cancel that read 'pending' while a
// runner claimed the job must not overwrite the claim.
func TestCancelDoesNotOverwriteAConcurrentClaim(t *testing.T) {
	f := newFx(t)
	f.job("p", nil)
	stale, _ := f.s.Get(f.ctx, "p") // the CLI's earlier read: pending
	c := f.running("p", 1)
	res, err := f.s.CancelJob(f.ctx, "p", f.now)
	if err != nil {
		t.Fatal(err)
	}
	if stale.Status != StatusPending || res.Status != StatusCancelling || !res.InFlight {
		t.Fatalf("stale = %s, result = %+v", stale.Status, res)
	}
	if j := f.get("p"); j.Status != StatusCancelling || j.ClaimToken != c.Token {
		t.Fatalf("cancel clobbered the claim: %+v", j)
	}
}

func TestCancelQuarantinedClosesOpenAttempt(t *testing.T) {
	f := newFx(t)
	f.job("q", nil)
	f.claim("q", 1)
	f.exec(`UPDATE scheduler_jobs SET status='quarantined', outcome_class='runner_inconsistent' WHERE id='q'`)
	res, err := f.s.CancelJob(f.ctx, "q", f.now)
	if err != nil || res.Status != StatusCancelled {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	if v := f.attemptStr("q", 1, "outcome_class"); v.String != ClassCancelled {
		t.Fatalf("attempt outcome = %v", v)
	}
	if v := f.attemptInt("q", 1, "ended_at"); !v.Valid {
		t.Fatal("attempt left open")
	}
	if j := f.get("q"); j.ClaimToken != "" || j.LeaseEpoch != nil {
		t.Fatalf("claim state not cleared: %+v", j)
	}
}

func TestCancelMissingJob(t *testing.T) {
	f := newFx(t)
	if _, err := f.s.CancelJob(f.ctx, "nope", f.now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestCancelledAttemptFinalizesCancelledAndNeverRetries(t *testing.T) {
	f := newFx(t)
	f.job("c", func(j *SpawnJob) {
		j.RetryPolicy = `{"max_attempts":5,"retry_on":["prestart","capacity"]}`
		j.Idempotent = true
	})
	c := f.running("c", 1)
	if _, err := f.s.CancelJob(f.ctx, "c", f.now); err != nil {
		t.Fatal(err)
	}
	// Even if the pipeline reported a retryable class, a cancelling job ends cancelled.
	res, err := f.s.Finalize(f.ctx, c, FinalizeParams{Class: ClassCapacity, Now: f.now})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusCancelled || res.Retry {
		t.Fatalf("res = %+v", res)
	}
	if v := f.attemptStr("c", 1, "outcome_class"); v.String != ClassCancelled {
		t.Fatalf("attempt outcome = %v", v)
	}
}

// --- Requeue / unblock -------------------------------------------------------

func TestRequeueNeedsConfirmAndClearsClaimState(t *testing.T) {
	f := newFx(t)
	f.job("q", nil)
	f.running("q", 1)
	f.exec(`UPDATE scheduler_jobs SET status='quarantined', outcome_class='legacy_unknown' WHERE id='q'`)
	if err := f.s.Requeue(f.ctx, "q", false, f.now); !errors.Is(err, ErrConfirmRequired) {
		t.Fatalf("err = %v, want ErrConfirmRequired", err)
	}
	if st := f.status("q"); st != StatusQuarantined {
		t.Fatalf("status = %s", st)
	}
	if err := f.s.Requeue(f.ctx, "q", true, f.now); err != nil {
		t.Fatal(err)
	}
	j := f.get("q")
	if j.Status != StatusPending || j.ClaimToken != "" || j.LeaseEpoch != nil || j.DispatchID != "" || j.OutcomeClass != "" {
		t.Fatalf("job = %+v", j)
	}
	if j.AttemptSeq != 1 {
		t.Fatalf("attempt_seq = %d, must never decrease", j.AttemptSeq)
	}
	if v := f.attemptStr("q", 1, "outcome_class"); v.String != ClassReleased {
		t.Fatalf("old attempt outcome = %v, want %s", v, ClassReleased)
	}
	// The job is claimable again, as attempt 2.
	if c := f.claim("q", 1); c.AttemptNo != 2 {
		t.Fatalf("attempt_no = %d, want 2", c.AttemptNo)
	}
}

func TestUnblockClearsRetryBlocked(t *testing.T) {
	f := newFx(t)
	f.job("u", nil)
	f.exec(`UPDATE scheduler_jobs SET retry_blocked='survivor_unknown:d1' WHERE id='u'`)
	if err := f.s.Unblock(f.ctx, "u", false); !errors.Is(err, ErrConfirmRequired) {
		t.Fatalf("err = %v, want ErrConfirmRequired", err)
	}
	if err := f.s.Unblock(f.ctx, "u", true); err != nil {
		t.Fatal(err)
	}
	if j := f.get("u"); j.RetryBlocked != "" {
		t.Fatalf("retry_blocked = %q", j.RetryBlocked)
	}
	if err := f.s.Unblock(f.ctx, "u", true); !errors.Is(err, ErrNotBlocked) {
		t.Fatalf("second unblock err = %v, want ErrNotBlocked", err)
	}
	f.claim("u", 1) // now claimable
}

// --- Unit 7: retry-policy validation ----------------------------------------

func TestRetryPolicyValidation(t *testing.T) {
	good := []struct {
		name, raw string
		idem      bool
	}{
		{"empty is the default", "", false},
		{"default literal", `{"max_attempts":1,"backoff":{"base_s":30,"factor":2,"max_s":1800,"jitter":0.2},"retry_on":["prestart"]}`, false},
		{"partial fills defaults", `{"max_attempts":3}`, false},
		{"empty retry_on never retries", `{"max_attempts":3,"retry_on":[]}`, false},
		{"capacity needs idempotent", `{"max_attempts":3,"retry_on":["prestart","capacity"]}`, true},
		{"indeterminate listed explicitly", `{"max_attempts":2,"retry_on":["indeterminate"]}`, true},
		{"jitter zero", `{"backoff":{"base_s":1,"factor":1,"max_s":1,"jitter":0}}`, false},
	}
	for _, tc := range good {
		if _, err := ParseRetryPolicy(tc.raw, tc.idem); err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	bad := []struct {
		name, raw string
		idem      bool
		want      error
	}{
		{"not json", `{`, false, ErrPolicyInvalid},
		{"unknown field", `{"max_attempt":3}`, false, ErrPolicyInvalid},
		{"trailing data", `{"max_attempts":1} {}`, false, ErrPolicyInvalid},
		{"max_attempts zero", `{"max_attempts":0}`, false, ErrPolicyInvalid},
		{"max_attempts negative", `{"max_attempts":-1}`, false, ErrPolicyInvalid},
		{"max_attempts too large", `{"max_attempts":1000}`, false, ErrPolicyInvalid},
		{"unknown class", `{"retry_on":["flaky"]}`, true, ErrPolicyInvalid},
		{"config_error never retryable", `{"retry_on":["config_error"]}`, true, ErrPolicyInvalid},
		{"cancelled never retryable", `{"retry_on":["cancelled"]}`, true, ErrPolicyInvalid},
		{"unknown never retryable", `{"retry_on":["unknown"]}`, true, ErrPolicyInvalid},
		{"ok is not a failure", `{"retry_on":["ok"]}`, true, ErrPolicyInvalid},
		{"capacity without idempotent", `{"retry_on":["capacity"]}`, false, ErrPolicyNeedsIdempotent},
		{"agent_failed without idempotent", `{"retry_on":["agent_failed"]}`, false, ErrPolicyNeedsIdempotent},
		{"indeterminate without idempotent", `{"retry_on":["indeterminate"]}`, false, ErrPolicyNeedsIdempotent},
		{"base zero", `{"backoff":{"base_s":0,"factor":2,"max_s":10,"jitter":0}}`, false, ErrPolicyInvalid},
		{"factor below one", `{"backoff":{"base_s":1,"factor":0.5,"max_s":10,"jitter":0}}`, false, ErrPolicyInvalid},
		{"max below base", `{"backoff":{"base_s":10,"factor":2,"max_s":5,"jitter":0}}`, false, ErrPolicyInvalid},
		{"jitter one", `{"backoff":{"base_s":1,"factor":2,"max_s":5,"jitter":1}}`, false, ErrPolicyInvalid},
		{"jitter negative", `{"backoff":{"base_s":1,"factor":2,"max_s":5,"jitter":-0.1}}`, false, ErrPolicyInvalid},
	}
	for _, tc := range bad {
		_, err := ParseRetryPolicy(tc.raw, tc.idem)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	p, err := ParseRetryPolicy("", false)
	if err != nil || p.MaxAttempts != 1 || p.Backoff.BaseS != 30 || p.Backoff.Factor != 2 ||
		p.Backoff.MaxS != 1800 || p.Backoff.Jitter != 0.2 || len(p.RetryOn) != 1 || p.RetryOn[0] != ClassPrestart {
		t.Fatalf("default = %+v, %v", p, err)
	}
}

// --- Unit 8: retry bounds, no refunds, backoff, crash cap -------------------

func TestBackoffBounds(t *testing.T) {
	p := RetryPolicy{MaxAttempts: 9, Backoff: Backoff{BaseS: 30, Factor: 2, MaxS: 1800, Jitter: 0.2}, RetryOn: []string{ClassPrestart}}
	lo := func() float64 { return 0 }
	mid := func() float64 { return 0.5 }
	hi := func() float64 { return 0.999999999 }
	prev := time.Duration(0)
	for seq := 1; seq <= 12; seq++ {
		base := 30.0
		for i := 1; i < seq; i++ {
			base *= 2
		}
		if base > 1800 {
			base = 1800
		}
		d0, d1, d2 := BackoffDelay(p, ClassPrestart, seq, lo), BackoffDelay(p, ClassPrestart, seq, mid), BackoffDelay(p, ClassPrestart, seq, hi)
		if d1 != time.Duration(base*float64(time.Second)) {
			t.Fatalf("seq %d mid = %v, want %vs", seq, d1, base)
		}
		if d0 < time.Duration(0.8*base*float64(time.Second))-time.Millisecond || d0 > d1 || d2 < d1 || d2 > time.Duration(1.2*base*float64(time.Second)) {
			t.Fatalf("seq %d jitter bounds: %v %v %v", seq, d0, d1, d2)
		}
		if d1 < prev {
			t.Fatalf("seq %d mid delay %v shrank from %v", seq, d1, prev)
		}
		prev = d1
	}
	// A seq below 1 is a caller bug; the delay is still never negative.
	if d := BackoffDelay(p, ClassPrestart, 0, lo); d < 0 {
		t.Fatalf("negative delay %v", d)
	}
	if d := BackoffDelay(p, ClassPrestart, -3, lo); d < 0 {
		t.Fatalf("negative delay %v", d)
	}
	// Huge exponents saturate at max_s rather than overflowing.
	if d := BackoffDelay(p, ClassPrestart, 5000, mid); d != 1800*time.Second {
		t.Fatalf("saturated delay = %v", d)
	}
}

func TestBackoffMinimumForCapacityAndUnclassified(t *testing.T) {
	p := RetryPolicy{MaxAttempts: 5, Backoff: Backoff{BaseS: 1, Factor: 1, MaxS: 1, Jitter: 0}}
	for _, class := range []string{ClassCapacity, ClassFailedUnclassified} {
		if d := BackoffDelay(p, class, 1, func() float64 { return 0.5 }); d != 15*time.Minute {
			t.Errorf("%s delay = %v, want the 15 minute floor", class, d)
		}
	}
	if d := BackoffDelay(p, ClassAgentFailed, 1, func() float64 { return 0.5 }); d != time.Second {
		t.Errorf("agent_failed delay = %v", d)
	}
	// The floor is a minimum, not a replacement.
	big := RetryPolicy{MaxAttempts: 5, Backoff: Backoff{BaseS: 3600, Factor: 1, MaxS: 3600, Jitter: 0}}
	if d := BackoffDelay(big, ClassCapacity, 1, func() float64 { return 0.5 }); d != time.Hour {
		t.Errorf("capacity with a larger base = %v", d)
	}
}

func TestShouldRetry(t *testing.T) {
	pol := func(max int, on ...string) RetryPolicy {
		return RetryPolicy{MaxAttempts: max, RetryOn: on}
	}
	cases := []struct {
		name          string
		p             RetryPolicy
		class         string
		seq, crashes  int
		blocked, want bool
	}{
		{"within limit", pol(3, ClassPrestart), ClassPrestart, 2, 0, false, true},
		{"exhausted", pol(3, ClassPrestart), ClassPrestart, 3, 0, false, false},
		{"default policy never retries a second time", pol(1, ClassPrestart), ClassPrestart, 1, 0, false, false},
		{"class not listed", pol(3, ClassPrestart), ClassAgentFailed, 1, 0, false, false},
		{"retry blocked", pol(3, ClassPrestart), ClassPrestart, 1, 0, true, false},
		{"excused crashes do not count", pol(2, ClassPrestart), ClassPrestart, 3, 2, false, true},
		{"excused crashes do not extend the limit", pol(2, ClassPrestart), ClassPrestart, 4, 2, false, false},
		{"indeterminate only when listed", pol(3, ClassPrestart), ClassIndeterminate, 1, 0, false, false},
		{"indeterminate listed", pol(3, ClassIndeterminate), ClassIndeterminate, 1, 0, false, true},
		{"config_error never", pol(3, ClassConfigError), ClassConfigError, 1, 0, false, false},
		{"cancelled never", pol(3, ClassCancelled), ClassCancelled, 1, 0, false, false},
		{"unknown never", pol(3, ClassUnknown), ClassUnknown, 1, 0, false, false},
		{"ok never", pol(3, ClassOK), ClassOK, 1, 0, false, false},
		{"abandoned never", pol(3, ClassAbandoned), ClassAbandoned, 1, 0, false, false},
	}
	for _, tc := range cases {
		if got := ShouldRetry(tc.p, tc.class, tc.seq, tc.crashes, tc.blocked); got != tc.want {
			t.Errorf("%s: ShouldRetry = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A prestart failure counts: a job with max_attempts 3 gets exactly three
// claims, each a new attempt, and the third failure is terminal.
func TestFinalizeCountsEveryAttemptNoRefunds(t *testing.T) {
	f := newFx(t)
	f.job("r", func(j *SpawnJob) {
		j.RetryPolicy = `{"max_attempts":3,"backoff":{"base_s":30,"factor":2,"max_s":1800,"jitter":0}}`
	})
	for seq := 1; seq <= 3; seq++ {
		now := f.now.Add(time.Duration(seq) * 24 * time.Hour)
		c, err := f.s.Claim(f.ctx, ClaimParams{JobID: "r", Epoch: 1, Now: now})
		if err != nil {
			t.Fatalf("claim %d: %v", seq, err)
		}
		if c.AttemptNo != seq {
			t.Fatalf("attempt_no = %d, want %d (never reused)", c.AttemptNo, seq)
		}
		res, err := f.s.Finalize(f.ctx, c, FinalizeParams{Class: ClassPrestart, Now: now, Rand: func() float64 { return 0.5 }})
		if err != nil {
			t.Fatalf("finalize %d: %v", seq, err)
		}
		if seq < 3 {
			wantDelay := time.Duration(30<<(seq-1)) * time.Second
			if res.Status != StatusPending || !res.Retry || !res.NotBefore.Equal(now.Add(wantDelay)) {
				t.Fatalf("seq %d: res = %+v, want pending at +%v", seq, res, wantDelay)
			}
			j := f.get("r")
			if j.ClaimToken != "" || j.LeaseEpoch != nil || j.DispatchID != "" || !j.NotBefore.Equal(now.Add(wantDelay)) {
				t.Fatalf("seq %d: claim state not cleared / not_before: %+v", seq, j)
			}
			// Not claimable before not_before.
			if _, err := f.s.Claim(f.ctx, ClaimParams{JobID: "r", Epoch: 1, Now: now}); !errors.Is(err, ErrLostClaim) {
				t.Fatalf("seq %d: claim before not_before: %v", seq, err)
			}
		} else if res.Status != StatusFailed || res.Retry {
			t.Fatalf("seq 3: res = %+v, want terminal failed", res)
		}
		if v := f.attemptStr("r", seq, "outcome_class"); v.String != ClassPrestart {
			t.Fatalf("attempt %d outcome = %v", seq, v)
		}
		if v := f.attemptInt("r", seq, "ended_at"); !v.Valid {
			t.Fatalf("attempt %d not ended", seq)
		}
	}
	j := f.get("r")
	if j.Status != StatusFailed || j.OutcomeClass != ClassPrestart || j.AttemptSeq != 3 || j.CompletedAt.IsZero() {
		t.Fatalf("job = %+v", j)
	}
}

func TestFinalizeTerminalMapping(t *testing.T) {
	cases := []struct {
		class string
		want  JobStatus
	}{
		{ClassOK, StatusCompleted},
		{ClassAgentFailed, StatusFailed},
		{ClassFailedUnclassified, StatusFailed},
		{ClassIndeterminate, StatusFailed},
		{ClassConfigError, StatusFailed},
		{ClassUnknown, StatusFailed},
		{ClassCancelled, StatusCancelled},
	}
	for _, tc := range cases {
		f := newFx(t)
		f.job("m", nil)
		c := f.running("m", 1)
		res, err := f.s.Finalize(f.ctx, c, FinalizeParams{Class: tc.class, Now: f.now, Error: "boom"})
		if err != nil {
			t.Fatalf("%s: %v", tc.class, err)
		}
		j := f.get("m")
		if res.Status != tc.want || j.Status != tc.want || j.OutcomeClass != tc.class || j.CompletedAt.IsZero() {
			t.Errorf("%s: res %+v job %+v", tc.class, res, j)
		}
		if tc.class != ClassOK && j.Error != "boom" {
			t.Errorf("%s: error_msg = %q", tc.class, j.Error)
		}
		if j.ClaimToken != "" || j.LeaseEpoch != nil || j.DispatchID != "" {
			t.Errorf("%s: claim state not cleared: %+v", tc.class, j)
		}
	}
	f := newFx(t)
	f.job("m", nil)
	c := f.running("m", 1)
	if _, err := f.s.Finalize(f.ctx, c, FinalizeParams{Class: "no-such-class", Now: f.now}); err == nil {
		t.Fatal("unknown outcome class accepted")
	}
	if st := f.status("m"); st != StatusRunning {
		t.Fatalf("a rejected class changed the job: %s", st)
	}
}

func TestFinalizeRetryBlockedIsTerminal(t *testing.T) {
	f := newFx(t)
	f.job("b", func(j *SpawnJob) {
		j.RetryPolicy = `{"max_attempts":5,"retry_on":["prestart","indeterminate"]}`
		j.Idempotent = true
	})
	c := f.running("b", 1)
	res, err := f.s.Finalize(f.ctx, c, FinalizeParams{Class: ClassIndeterminate, Now: f.now, RetryBlocked: "survivor_unknown:d-b"})
	if err != nil {
		t.Fatal(err)
	}
	j := f.get("b")
	if res.Retry || j.Status != StatusFailed || j.RetryBlocked != "survivor_unknown:d-b" {
		t.Fatalf("res %+v job %+v", res, j)
	}
}

func TestFinalizeRejectsStaleEpochAndForgedToken(t *testing.T) {
	f := newFx(t)
	f.job("s", nil)
	c := f.running("s", 1)
	forged := *c
	forged.Token = "forged"
	if _, err := f.s.Finalize(f.ctx, &forged, FinalizeParams{Class: ClassOK, Now: f.now}); !errors.Is(err, ErrLostClaim) {
		t.Fatalf("forged token: %v", err)
	}
	f.setEpoch(2) // a newer runner took over
	if _, err := f.s.Finalize(f.ctx, c, FinalizeParams{Class: ClassOK, Now: f.now}); !errors.Is(err, ErrLostClaim) {
		t.Fatalf("stale epoch: %v", err)
	}
	if st := f.status("s"); st != StatusRunning {
		t.Fatalf("status = %s", st)
	}
	if v := f.attemptInt("s", 1, "ended_at"); v.Valid {
		t.Fatal("a rejected finalize ended the attempt")
	}
}

// The runner dies before linking, four times in a row: three excused requeues,
// then abandoned with the counter capped at 3.
func TestRecoverPrestartAbortCapsAtThreeThenAbandons(t *testing.T) {
	f := newFx(t)
	f.job("k", func(j *SpawnJob) { j.RetryPolicy = `{"max_attempts":1}` })
	for crash := 1; crash <= 4; crash++ {
		epoch := int64(crash)
		f.setEpoch(epoch)
		c := f.claim("k", epoch)
		f.setEpoch(epoch + 1) // the runner died; its successor recovers
		res, err := f.s.Recover(f.ctx, RecoverParams{JobID: "k", Epoch: epoch + 1, Action: ActPrestartAbort, Now: f.now})
		if err != nil {
			t.Fatalf("crash %d: %v", crash, err)
		}
		j := f.get("k")
		if v := f.attemptStr("k", c.AttemptNo, "outcome_class"); v.String != ClassPrestartAbort {
			t.Fatalf("crash %d attempt outcome = %v", crash, v)
		}
		if v := f.attemptInt("k", c.AttemptNo, "counts_toward_limit"); v.Int64 != 0 {
			t.Fatalf("crash %d: the excused attempt still counts", crash)
		}
		if crash < 4 {
			if res.Status != StatusPending || j.Status != StatusPending || j.CrashRequeues != crash || j.AttemptSeq != crash {
				t.Fatalf("crash %d: res %+v job %+v", crash, res, j)
			}
			if j.ClaimToken != "" || j.LeaseEpoch != nil {
				t.Fatalf("crash %d: claim state not cleared", crash)
			}
		} else if res.Status != StatusFailed || j.Status != StatusFailed || j.OutcomeClass != ClassAbandoned || j.CrashRequeues != 3 || j.AttemptSeq != 4 {
			t.Fatalf("crash 4: res %+v job %+v, want failed/abandoned with crash_requeues 3", res, j)
		}
	}
}

// A persisted launch_result survives the crash and the attempt counts.
func TestRecoverCountsAPersistedRejection(t *testing.T) {
	f := newFx(t)
	f.job("r", nil) // default policy: max_attempts 1
	c := f.claim("r", 1)
	if ok, err := f.s.RecordLaunchResult(f.ctx, c, "rejected"); err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	f.setEpoch(2)
	res, err := f.s.Recover(f.ctx, RecoverParams{JobID: "r", Epoch: 2, Action: ActFinalize, Class: ClassPrestart, Now: f.now})
	if err != nil {
		t.Fatal(err)
	}
	j := f.get("r")
	if res.Status != StatusFailed || j.CrashRequeues != 0 || j.OutcomeClass != ClassPrestart {
		t.Fatalf("res %+v job %+v: a persisted rejection is a counted failure, not an excused crash", res, j)
	}
	if v := f.attemptInt("r", 1, "counts_toward_limit"); v.Int64 != 1 {
		t.Fatal("counted attempt lost its count")
	}
}

// --- Recover transaction ----------------------------------------------------

func TestRecoverTouchesOnlyPriorEpochRows(t *testing.T) {
	f := newFx(t)
	f.job("own", nil)
	f.claim("own", 1)
	// Recovery by epoch 1 must not touch epoch-1 rows (it is that runner's own).
	if _, err := f.s.Recover(f.ctx, RecoverParams{JobID: "own", Epoch: 1, Action: ActCancelled, Now: f.now}); !errors.Is(err, ErrLostClaim) {
		t.Fatalf("own-epoch recover err = %v", err)
	}
	// A recovery from an epoch that is not the current one is stale.
	if _, err := f.s.Recover(f.ctx, RecoverParams{JobID: "own", Epoch: 5, Action: ActCancelled, Now: f.now}); !errors.Is(err, ErrLostClaim) {
		t.Fatalf("stale-epoch recover err = %v", err)
	}
	if st := f.status("own"); st != StatusLaunching {
		t.Fatalf("status = %s", st)
	}
	// Terminal and pending rows are never recovered.
	f.job("p", nil)
	f.setEpoch(2)
	if _, err := f.s.Recover(f.ctx, RecoverParams{JobID: "p", Epoch: 2, Action: ActCancelled, Now: f.now}); !errors.Is(err, ErrLostClaim) {
		t.Fatalf("pending recover err = %v", err)
	}
}

func TestRecoverActionsLeaveAllowedStates(t *testing.T) {
	type tc struct {
		name   string
		build  func(f *fx, id string)
		p      RecoverParams
		status JobStatus
		class  string
	}
	cases := []tc{
		{"cancelling unlinked", func(f *fx, id string) {
			f.claim(id, 1)
			if _, err := f.s.CancelJob(f.ctx, id, f.now); err != nil {
				t.Fatal(err)
			}
		}, RecoverParams{Action: ActCancelled}, StatusCancelled, ClassCancelled},
		{"unlinked launching, runner died", func(f *fx, id string) { f.claim(id, 1) },
			RecoverParams{Action: ActPrestartAbort}, StatusPending, ""},
		{"linked terminal dispatch, ok", func(f *fx, id string) { f.running(id, 1) },
			RecoverParams{Action: ActFinalize, Class: ClassOK}, StatusCompleted, ClassOK},
		{"linked terminal dispatch, failed", func(f *fx, id string) { f.running(id, 1) },
			RecoverParams{Action: ActFinalize, Class: ClassAgentFailed}, StatusFailed, ClassAgentFailed},
		{"inconsistent", func(f *fx, id string) { f.claim(id, 1) },
			RecoverParams{Action: ActQuarantine, Reason: ReasonRunnerInconsistent}, StatusQuarantined, ReasonRunnerInconsistent},
		{"survivor in fallback mode", func(f *fx, id string) { f.running(id, 1) },
			RecoverParams{Action: ActQuarantine, Reason: ReasonSurvivorUnknown}, StatusQuarantined, ReasonSurvivorUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFx(t)
			f.job("j", nil)
			c.build(f, "j")
			f.setEpoch(2)
			c.p.JobID, c.p.Epoch, c.p.Now = "j", 2, f.now
			res, err := f.s.Recover(f.ctx, c.p)
			if err != nil {
				t.Fatal(err)
			}
			j := f.get("j")
			if res.Status != c.status || j.Status != c.status {
				t.Fatalf("res %+v job %+v, want %s", res, j, c.status)
			}
			if c.class != "" && j.OutcomeClass != c.class {
				t.Fatalf("outcome_class = %q, want %q", j.OutcomeClass, c.class)
			}
			if j.Status == StatusQuarantined {
				// Quarantine keeps the evidence an operator needs.
				if j.ClaimToken == "" {
					t.Fatal("quarantine dropped the claim token")
				}
			}
			if j.Status != StatusQuarantined && (j.ClaimToken != "" || j.LeaseEpoch != nil || j.DispatchID != "") {
				t.Fatalf("claim state not cleared: %+v", j)
			}
		})
	}
}

func TestRecoverQuarantineNeedsAKnownReason(t *testing.T) {
	f := newFx(t)
	f.job("j", nil)
	f.claim("j", 1)
	f.setEpoch(2)
	if _, err := f.s.Recover(f.ctx, RecoverParams{JobID: "j", Epoch: 2, Action: ActQuarantine, Reason: "because", Now: f.now}); err == nil {
		t.Fatal("unknown quarantine reason accepted")
	}
	if st := f.status("j"); st != StatusLaunching {
		t.Fatalf("status = %s", st)
	}
}

// The legacy rule (section 3): ok completes, every other definite class fails
// with no retry, and an indeterminate or unknown class quarantines.
func TestRecoverLegacyAttemptLimits(t *testing.T) {
	cases := []struct {
		class  string
		status JobStatus
		out    string
	}{
		{ClassOK, StatusCompleted, ClassOK},
		{ClassAgentFailed, StatusFailed, ClassAgentFailed},
		{ClassCapacity, StatusFailed, ClassCapacity},
		{ClassPrestart, StatusFailed, ClassPrestart},
		{ClassIndeterminate, StatusQuarantined, ReasonLegacyUnknown},
		{ClassUnknown, StatusQuarantined, ReasonLegacyUnknown},
	}
	for _, tc := range cases {
		f := newFx(t)
		f.job("lg", func(j *SpawnJob) {
			j.RetryPolicy = `{"max_attempts":9,"retry_on":["prestart","capacity","indeterminate"]}`
			j.Idempotent = true
		})
		c := f.claim("lg", 1)
		f.exec(`UPDATE scheduler_job_attempts SET legacy=1 WHERE claim_token=?`, c.Token)
		f.setEpoch(2)
		res, err := f.s.Recover(f.ctx, RecoverParams{JobID: "lg", Epoch: 2, Action: ActFinalize, Class: tc.class, Now: f.now})
		if err != nil {
			t.Fatalf("%s: %v", tc.class, err)
		}
		j := f.get("lg")
		if res.Status != tc.status || j.Status != tc.status || j.OutcomeClass != tc.out || res.Retry {
			t.Errorf("%s: res %+v job %+v, want %s/%s", tc.class, res, j, tc.status, tc.out)
		}
	}
}

// --- Unit 6b: recovery decision table ---------------------------------------

func TestRecoveryDecisionIsTotalAndMatchesTheTable(t *testing.T) {
	statuses := []JobStatus{StatusLaunching, StatusCancelling, StatusRunning}
	results := []string{"", LaunchRejected, LaunchHookAborted, LaunchSpawnError, LaunchStarted}
	disps := []DispatchState{DispAbsent, DispTerminal, DispPruned, DispLivePID, DispLiveNoPID}
	n := 0
	for _, status := range statuses {
		for _, legacy := range []bool{false, true} {
			for _, linked := range []bool{false, true} {
				for _, lr := range results {
					for _, disp := range disps {
						n++
						in := RecoveryInput{Status: status, Legacy: legacy, Linked: linked, LaunchResult: lr, Dispatch: disp}
						got := DecideRecovery(in)
						name := string(status) + "/legacy=" + boolStr(legacy) + "/linked=" + boolStr(linked) + "/lr=" + lr + "/disp=" + disp.String()
						if got.Row < 1 || got.Row > 9 || got.Action == ActNone {
							t.Fatalf("%s: not total: %+v", name, got)
						}
						wantRow, wantAct, wantReason := expectedRecovery(in)
						if got.Row != wantRow || got.Action != wantAct || got.Reason != wantReason {
							t.Errorf("%s: got row %d %v %q, want row %d %v %q", name, got.Row, got.Action, got.Reason, wantRow, wantAct, wantReason)
						}
						reachable := recoveryReachable(in)
						if reachable && got.Reason == ReasonRunnerInconsistent {
							t.Errorf("%s: a reachable tuple was quarantined as inconsistent", name)
						}
						if !reachable && !in.Legacy {
							// Impossible tuples get exactly the listed conservative disposition.
							switch {
							case !in.Linked && in.Status == StatusLaunching && in.LaunchResult == LaunchStarted && in.Dispatch == DispAbsent:
								if got.Row != 5 || got.Action != ActQuarantine || got.Reason != ReasonRunnerInconsistent {
									t.Errorf("%s: %+v", name, got)
								}
							case !in.Linked && in.Status == StatusCancelling && in.LaunchResult == LaunchStarted && in.Dispatch == DispAbsent:
								if got.Row != 2 || got.Action != ActCancelled {
									t.Errorf("%s: %+v", name, got)
								}
							default:
								if got.Row != 9 || got.Action != ActQuarantine || got.Reason != ReasonRunnerInconsistent {
									t.Errorf("%s: %+v", name, got)
								}
							}
						}
					}
				}
			}
		}
	}
	if n != 3*2*2*5*5 {
		t.Fatalf("enumerated %d tuples", n)
	}
}

func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// recoveryReachable restates the transition invariants of 5.3d independently
// of the table: unlinked rows have no dispatch; `started` and `running` imply
// a link; linked rows have a dispatch (terminal, pruned or live).
func recoveryReachable(in RecoveryInput) bool {
	if in.Legacy {
		return true
	}
	if !in.Linked {
		if in.Dispatch != DispAbsent {
			return false
		}
		if in.Status == StatusRunning {
			return false
		}
		return in.LaunchResult != LaunchStarted
	}
	return in.Dispatch != DispAbsent
}

// expectedRecovery lists the table rows of 5.3d as plain predicates, first match.
func expectedRecovery(in RecoveryInput) (int, RecoveryAction, string) {
	switch {
	case in.Legacy:
		return 1, ActLegacy, ""
	case !in.Linked && in.Dispatch == DispAbsent && in.Status == StatusCancelling:
		return 2, ActCancelled, ""
	case !in.Linked && in.Dispatch == DispAbsent && in.Status == StatusLaunching && in.LaunchResult == "":
		return 3, ActPrestartAbort, ""
	case !in.Linked && in.Dispatch == DispAbsent && in.Status == StatusLaunching &&
		(in.LaunchResult == LaunchRejected || in.LaunchResult == LaunchHookAborted || in.LaunchResult == LaunchSpawnError):
		return 4, ActFinalize, ClassPrestart
	case !in.Linked && in.Dispatch == DispAbsent && in.Status == StatusLaunching && in.LaunchResult == LaunchStarted:
		return 5, ActQuarantine, ReasonRunnerInconsistent
	case in.Linked && in.Dispatch == DispTerminal:
		return 6, ActPipeline, ""
	case in.Linked && in.Dispatch == DispPruned:
		return 6, ActPipeline, ClassIndeterminate
	case in.Linked && in.Dispatch == DispLivePID:
		return 7, ActCheckPID, ""
	case in.Linked && in.Dispatch == DispLiveNoPID:
		return 8, ActSurvivor, ""
	default:
		return 9, ActQuarantine, ReasonRunnerInconsistent
	}
}

func TestInRecoveryScope(t *testing.T) {
	zero, one := int64(0), int64(1)
	cases := []struct {
		status JobStatus
		lease  *int64
		mine   int64
		want   bool
	}{
		{StatusRunning, &zero, 1, true}, // legacy rows migrate with epoch 0
		{StatusLaunching, nil, 1, true}, // a NULL epoch is epoch 0
		{StatusCancelling, &one, 2, true},
		{StatusRunning, &one, 1, false}, // this runner's own rows are never recovered
		{StatusRunning, &one, 0, false},
		{StatusPending, &zero, 1, false},
		{StatusQuarantined, &zero, 1, false},
		{StatusCompleted, &zero, 1, false},
	}
	for _, tc := range cases {
		if got := InRecoveryScope(tc.status, tc.lease, tc.mine); got != tc.want {
			t.Errorf("%s lease=%v mine=%d: %v, want %v", tc.status, tc.lease, tc.mine, got, tc.want)
		}
	}
}

func TestNewStatusesAreNotTerminal(t *testing.T) {
	for _, st := range []JobStatus{StatusLaunching, StatusCancelling, StatusQuarantined} {
		j := NewSpawnJob("x", JobTypeDispatch, "s")
		j.SetStatus(st)
		if j.IsTerminal() {
			t.Errorf("%s reported terminal", st)
		}
	}
	if !strings.Contains(string(StatusQuarantined), "quarantined") {
		t.Fatal("status spelling")
	}
}
