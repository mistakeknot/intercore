package scheduler

import (
	"errors"
	"testing"
	"time"
)

func ident(path string) RunnerIdentity {
	return RunnerIdentity{
		RunnerID: "r1", PID: 4242, PIDStartTime: 99, BootID: "boot", Host: "h", Version: "t",
		DBPath: path, DBIdentity: "7:11", NetNS: 4026531840, UID: 1000,
	}
}

func TestAcquireEpochBumpsAndRecords(t *testing.T) {
	f := newFx(t)
	e1, prior, err := f.s.AcquireEpoch(f.ctx, ident("/d/a.db"), f.now)
	if err != nil {
		t.Fatal(err)
	}
	if e1 != 2 || prior.Epoch != 1 || prior.DBPath != "" {
		t.Fatalf("first acquire: epoch %d prior %+v", e1, prior)
	}
	e2, prior2, err := f.s.AcquireEpoch(f.ctx, ident("/d/a.db"), f.now.Add(time.Second))
	if err != nil || e2 != 3 || prior2.PID != 4242 || prior2.DBIdentity != "7:11" {
		t.Fatalf("second acquire: %d %+v %v", e2, prior2, err)
	}
	row, err := f.s.ReadRunner(f.ctx)
	if err != nil || row.Epoch != 3 || row.State != "running" || row.QueueID == "" {
		t.Fatalf("row %+v %v", row, err)
	}
}

// Unit test 5: a write made with a stale epoch changes nothing.
func TestEpochGuardRefusesStaleRunner(t *testing.T) {
	f := newFx(t)
	f.job("j1", nil)
	e, _, err := f.s.AcquireEpoch(f.ctx, ident("/d/a.db"), f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Claim(f.ctx, ClaimParams{JobID: "j1", Epoch: e - 1, Now: f.now}); !errors.Is(err, ErrLostClaim) {
		t.Fatalf("stale claim: %v", err)
	}
	if got := f.status("j1"); got != StatusPending {
		t.Fatalf("stale claim moved the job to %s", got)
	}
	c, err := f.s.Claim(f.ctx, ClaimParams{JobID: "j1", Epoch: e, Now: f.now})
	if err != nil {
		t.Fatal(err)
	}
	// A newer runner takes the epoch; every write of the old claim is refused.
	if _, _, err := f.s.AcquireEpoch(f.ctx, ident("/d/a.db"), f.now); err != nil {
		t.Fatal(err)
	}
	if ok, err := f.s.RecordLaunchResult(f.ctx, c, LaunchStarted); err != nil || ok {
		t.Fatalf("old epoch recorded a launch result: %v %v", ok, err)
	}
	if ok, _ := f.s.MarkExited(f.ctx, c, f.now); ok {
		t.Fatal("old epoch marked exit")
	}
	if _, err := f.s.Finalize(f.ctx, c, FinalizeParams{Class: ClassOK, Now: f.now}); !errors.Is(err, ErrLostClaim) {
		t.Fatalf("old epoch finalized: %v", err)
	}
	if ok, _ := f.s.Heartbeat(f.ctx, e, f.now); ok {
		t.Fatal("old epoch heartbeat succeeded")
	}
	if ok, _ := f.s.SetRetryBlocked(f.ctx, c, "x", f.now); ok {
		t.Fatal("old epoch set retry_blocked")
	}
	if ok, _ := f.s.FailConfig(f.ctx, "j1", e, "x", f.now); ok {
		t.Fatal("old epoch failed a job")
	}
}

func TestAcquireRefusesIdentityMismatch(t *testing.T) {
	f := newFx(t)
	if _, _, err := f.s.AcquireEpoch(f.ctx, ident("/d/a.db"), f.now); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*RunnerIdentity){
		"path":     func(i *RunnerIdentity) { i.DBPath = "/d/b.db" },
		"identity": func(i *RunnerIdentity) { i.DBIdentity = "7:12" },
		"netns":    func(i *RunnerIdentity) { i.NetNS++ },
		"uid":      func(i *RunnerIdentity) { i.UID++ },
	} {
		id := ident("/d/a.db")
		mut(&id)
		_, _, err := f.s.AcquireEpoch(f.ctx, id, f.now)
		if !errors.Is(err, ErrIdentityMismatch) {
			t.Errorf("%s: want ErrIdentityMismatch, got %v", name, err)
		}
	}
	row, _ := f.s.ReadRunner(f.ctx)
	if row.Epoch != 2 {
		t.Fatalf("a refused acquire bumped the epoch to %d", row.Epoch)
	}
}

func TestHeartbeatTracksEpoch(t *testing.T) {
	f := newFx(t)
	e, _, _ := f.s.AcquireEpoch(f.ctx, ident("/d/a.db"), f.now)
	if ok, err := f.s.Heartbeat(f.ctx, e, f.now.Add(5*time.Second)); !ok || err != nil {
		t.Fatalf("heartbeat: %v %v", ok, err)
	}
	row, _ := f.s.ReadRunner(f.ctx)
	if row.HeartbeatAt != f.now.Add(5*time.Second).Unix() {
		t.Fatalf("heartbeat_at %d", row.HeartbeatAt)
	}
	f.setEpoch(e + 1)
	if ok, _ := f.s.Heartbeat(f.ctx, e, f.now); ok {
		t.Fatal("heartbeat at a superseded epoch changed a row")
	}
}

func TestRebindQuarantinesInFlightAndPauses(t *testing.T) {
	f := newFx(t)
	f.job("run", nil)
	f.job("pend", nil)
	e, _, _ := f.s.AcquireEpoch(f.ctx, ident("/d/a.db"), f.now)
	c, err := f.s.Claim(f.ctx, ClaimParams{JobID: "run", Epoch: e, Now: f.now})
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.s.Rebind(f.ctx, RebindParams{Identity: ident("/e/c.db"), Now: f.now})
	if err != nil {
		t.Fatal(err)
	}
	if res.Epoch != e+1 || res.Quarantined != 1 {
		t.Fatalf("rebind result %+v", res)
	}
	if got := f.status("run"); got != StatusQuarantined {
		t.Fatalf("in-flight row is %s", got)
	}
	if got := f.get("run").OutcomeClass; got != ReasonRebindUnknown {
		t.Fatalf("outcome_class %q", got)
	}
	if f.attemptInt("run", 1, "ended_at").Valid {
		t.Fatal("rebind closed the attempt; it is evidence for requeue")
	}
	if f.status("pend") != StatusPending {
		t.Fatal("rebind touched a pending row")
	}
	if paused, _ := f.s.IsPaused(f.ctx); !paused {
		t.Fatal("rebind did not pause the scheduler")
	}
	row, _ := f.s.ReadRunner(f.ctx)
	if row.DBPath != "/e/c.db" || row.DBIdentity != "7:11" || row.Epoch != e+1 {
		t.Fatalf("row after rebind %+v", row)
	}
	// The old claim is dead, and the new identity now matches.
	if _, err := f.s.Finalize(f.ctx, c, FinalizeParams{Class: ClassOK, Now: f.now}); !errors.Is(err, ErrLostClaim) {
		t.Fatalf("finalize of a quarantined claim: %v", err)
	}
	if _, _, err := f.s.AcquireEpoch(f.ctx, ident("/e/c.db"), f.now); err != nil {
		t.Fatalf("acquire after rebind: %v", err)
	}
}

// Unit test 12: claim order.
func TestClaimCandidatesOrder(t *testing.T) {
	f := newFx(t)
	f.job("late-hi", func(j *SpawnJob) { j.Priority = PriorityHigh; j.CreatedAt = f.now.Add(10 * time.Second) })
	f.job("early-lo", func(j *SpawnJob) { j.Priority = PriorityLow; j.CreatedAt = f.now })
	f.job("b-norm", func(j *SpawnJob) { j.CreatedAt = f.now.Add(2 * time.Second) })
	f.job("a-norm", func(j *SpawnJob) { j.CreatedAt = f.now.Add(2 * time.Second) })
	f.job("future", func(j *SpawnJob) { j.NotBefore = f.now.Add(time.Hour); j.Priority = PriorityHigh })
	f.job("blocked", func(j *SpawnJob) { j.Priority = PriorityHigh })
	f.exec(`UPDATE scheduler_jobs SET retry_blocked = 'survivor_unknown:x' WHERE id = 'blocked'`)
	got, err := f.s.ClaimCandidates(f.ctx, f.now.Add(time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"late-hi", "a-norm", "b-norm", "early-lo"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

func TestFailConfigIsGuardedAndWritesNoAttempt(t *testing.T) {
	f := newFx(t)
	f.job("j", nil)
	e, _, _ := f.s.AcquireEpoch(f.ctx, ident("/d/a.db"), f.now)
	f.exec(`INSERT INTO state (key, scope_id, payload, updated_at) VALUES ('scheduler_paused', 'global', 'true', 1)`)
	if ok, _ := f.s.FailConfig(f.ctx, "j", e, "boom", f.now); ok {
		t.Fatal("FailConfig acted on a paused queue")
	}
	f.exec(`UPDATE state SET payload = 'false' WHERE key = 'scheduler_paused'`)
	if ok, err := f.s.FailConfig(f.ctx, "j", e, "boom", f.now); !ok || err != nil {
		t.Fatalf("FailConfig: %v %v", ok, err)
	}
	j := f.get("j")
	if j.Status != StatusFailed || j.OutcomeClass != ClassConfigError || j.Error != "boom" {
		t.Fatalf("job %+v", j)
	}
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM scheduler_job_attempts WHERE job_id = 'j'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("attempt rows %d %v", n, err)
	}
	if ok, _ := f.s.FailConfig(f.ctx, "j", e, "again", f.now); ok {
		t.Fatal("FailConfig acted on a terminal job")
	}
}

func TestAttemptByTokenAndSetPaths(t *testing.T) {
	f := newFx(t)
	f.job("j", nil)
	e, _, _ := f.s.AcquireEpoch(f.ctx, ident("/d/a.db"), f.now)
	c, err := f.s.Claim(f.ctx, ClaimParams{JobID: "j", Epoch: e, Now: f.now})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := f.s.SetAttemptPaths(f.ctx, c, "/o/out.j.a1", "cgroup", "/sys/fs/cgroup/x/attempt-j-1"); !ok || err != nil {
		t.Fatalf("paths: %v %v", ok, err)
	}
	a, err := f.s.AttemptByToken(f.ctx, c.Token)
	if err != nil {
		t.Fatal(err)
	}
	if a.OutputFile != "/o/out.j.a1" || a.Containment != "cgroup" || a.CgroupPath == "" || a.JobStatus != StatusLaunching || a.JobEpoch != e {
		t.Fatalf("attempt %+v", a)
	}
	if _, err := f.s.AttemptByToken(f.ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing token: %v", err)
	}
	got, err := f.s.ClaimFor(f.ctx, c.Token, e)
	if err != nil || got.JobID != "j" || got.AttemptNo != 1 {
		t.Fatalf("ClaimFor %+v %v", got, err)
	}
	if _, err := f.s.ClaimFor(f.ctx, c.Token, e+1); !errors.Is(err, ErrLostClaim) {
		t.Fatalf("ClaimFor at another epoch: %v", err)
	}
}
