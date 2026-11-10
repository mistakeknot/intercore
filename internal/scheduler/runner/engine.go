//go:build icsched_test && linux

package runner

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mistakeknot/intercore/internal/dispatch"
	"github.com/mistakeknot/intercore/internal/scheduler"
)

var (
	errNotOurs = errors.New("the row is no longer ours")
	errAborted = errors.New("runner is stopping")
)

// engine is the claim and launch loop of slice 3b. One goroutine (run) owns
// the claim, cancel, completion and repair steps. Each launched job has a
// launch goroutine and, once its process started, exactly one waiter. The
// in-flight map is the only link between them; every entry reaches completing
// and only the main loop removes it, after the finalize commits.
//
// All database work uses context.Background(): the run context is cancelled
// to start a drain, and the drain still has to finish what is in flight.
type engine struct {
	o     Options
	store *scheduler.Store
	ds    *dispatch.Store
	sqlDB *sql.DB
	epoch int64
	fl    *InFlight

	mu      sync.Mutex
	backoff map[string]backoffState // completing entries that failed a step
	killed  map[string]bool         // cancelling tokens whose dispatch was signalled
	killing map[string]bool         // cancelling tokens with a signal in progress

	stopping atomic.Bool
	abort    chan struct{}
	fatal    chan error
	wake     chan struct{}
}

type backoffState struct {
	next  time.Time
	delay time.Duration
}

func newEngine(o Options, store *scheduler.Store, sqlDB *sql.DB, epoch int64) *engine {
	return &engine{
		o:       o,
		store:   store,
		ds:      dispatch.New(sqlDB, nil),
		sqlDB:   sqlDB,
		epoch:   epoch,
		fl:      NewInFlight(),
		backoff: map[string]backoffState{},
		killed:  map[string]bool{},
		killing: map[string]bool{},
		abort:   make(chan struct{}),
		fatal:   make(chan error, 1),
		wake:    make(chan struct{}, 1),
	}
}

func (e *engine) logf(format string, a ...any) {
	fmt.Fprintf(e.o.Stderr, "ic scheduler run: "+format+"\n", a...)
}

func (e *engine) inj(kind string) error {
	if h := e.o.hooks.inject; h != nil {
		return h(kind)
	}
	return nil
}

func (e *engine) signal() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

func newClaimToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// checkEpoch implements the fenced-write rule: after a write that matched no
// row, the singleton decides. If the epoch is not ours the runner was
// superseded and must exit 3. Anything else (a cancelling job, a changed
// token) is ordinary contention and never stops the runner.
func (e *engine) checkEpoch() {
	cur, err := e.store.CurrentEpoch(context.Background())
	if err != nil {
		e.logf("epoch check: %v", err)
		return
	}
	if cur != e.epoch {
		select {
		case e.fatal <- exitf(ExitSuperseded, "superseded: epoch %d is no longer current (now %d)", e.epoch, cur):
		default:
		}
	}
}

// lost handles a row that is not ours: decide whether we were superseded, and
// forget the entry either way so the repair pass has nothing to fight over.
func (e *engine) lost(token string) {
	e.checkEpoch()
	e.fl.Drop(token)
}

// run is the main loop. It returns nil after a clean drain, or an *ExitError.
func (e *engine) run(ctx context.Context) error {
	defer close(e.abort)
	if h := e.o.hooks.onInFlight; h != nil {
		h(e.fl)
	}
	tick := time.NewTicker(e.o.Tick)
	defer tick.Stop()
	hb := time.NewTicker(e.o.HeartbeatEvery)
	defer hb.Stop()
	hbErrs := 0
	nextRepair := time.Now().Add(e.o.RepairEvery)
	for {
		select {
		case <-ctx.Done():
			return e.drainAndStop()
		case err := <-e.fatal:
			return err
		case <-hb.C:
			if err := e.heartbeat(&hbErrs); err != nil {
				return err
			}
		case <-e.wake:
			e.completeStep()
		case <-tick.C:
			e.cancelStep()
			e.completeStep()
			if !time.Now().Before(nextRepair) {
				e.repairStep()
				nextRepair = time.Now().Add(e.o.RepairEvery)
			}
			e.claimStep()
		}
		select {
		case err := <-e.fatal:
			return err
		default:
		}
	}
}

func (e *engine) heartbeat(errs *int) error {
	ok, err := e.store.Heartbeat(context.Background(), e.epoch, e.o.Now())
	switch {
	case err != nil:
		*errs++
		e.logf("heartbeat error (%d): %v", *errs, err)
		if *errs >= maxHeartbeatErrors {
			return exitf(ExitRefused, "heartbeat failed %d times in a row: %w", *errs, err)
		}
	case !ok:
		return exitf(ExitSuperseded, "superseded: epoch %d is no longer current", e.epoch)
	default:
		*errs = 0
	}
	return nil
}

// drainAndStop stops claiming, lets in-flight jobs finish for DrainTimeout
// (completing and heartbeating the whole time), then marks the runner stopped.
// Children still running at the deadline are left to the next runner's
// recovery; they run in their own process group and are not signalled.
func (e *engine) drainAndStop() error {
	e.stopping.Store(true)
	dctx, cancel := context.WithTimeout(context.Background(), e.o.BusyTimeout)
	_, err := e.store.SetRunnerState(dctx, e.epoch, "draining")
	cancel()
	if err != nil {
		return exitf(ExitRefused, "drain: could not mark the runner draining; the singleton row still says running: %w", err)
	}
	deadline := time.NewTimer(e.o.DrainTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(e.o.Tick)
	defer tick.Stop()
	hb := time.NewTicker(e.o.HeartbeatEvery)
	defer hb.Stop()
	hbErrs := 0
	for e.fl.Len() > 0 {
		select {
		case <-deadline.C:
			e.logf("drain: %d job(s) still in flight at the drain timeout; leaving them to recovery", e.fl.Len())
			goto stopped
		case err := <-e.fatal:
			return err
		case <-hb.C:
			if err := e.heartbeat(&hbErrs); err != nil {
				return err
			}
		case <-e.wake:
			e.completeStep()
		case <-tick.C:
			e.cancelStep()
			e.completeStep()
		}
	}
stopped:
	dctx, cancel = context.WithTimeout(context.Background(), e.o.BusyTimeout)
	defer cancel()
	if _, err := e.store.SetRunnerState(dctx, e.epoch, "stopped"); err != nil {
		return exitf(ExitRefused, "drain: could not mark the runner stopped; the singleton row still says running: %w", err)
	}
	return nil
}

// claimStep claims due jobs while there is capacity. A job that fails a
// claim-time check ends config_error and uses no capacity.
func (e *engine) claimStep() {
	if e.stopping.Load() {
		return
	}
	ctx := context.Background()
	for {
		paused, err := e.store.IsPaused(ctx)
		if err != nil {
			e.logf("claim: pause check: %v", err)
			return
		}
		if h := e.o.hooks.claimPass; h != nil {
			h(paused)
		}
		if paused {
			return
		}
		free := e.o.MaxConcurrent - e.fl.Len()
		if free <= 0 {
			return
		}
		ids, err := e.store.ClaimCandidates(ctx, e.o.Now(), free)
		if err != nil {
			e.logf("claim: candidates: %v", err)
			return
		}
		if len(ids) == 0 {
			return
		}
		if h := e.o.hooks.afterCandidates; h != nil {
			h(ids)
		}
		progress := false
		for _, id := range ids {
			if e.fl.Len() >= e.o.MaxConcurrent {
				break
			}
			if e.tryClaim(ctx, id) {
				progress = true
			}
		}
		if !progress {
			return
		}
	}
}

// tryClaim reports whether the job made progress (claimed, or failed as a
// config error). A lost claim is normal contention and is not progress.
func (e *engine) tryClaim(ctx context.Context, id string) bool {
	job, err := e.store.Get(ctx, id)
	if err != nil {
		if !errors.Is(err, scheduler.ErrNotFound) {
			e.logf("claim %s: read job: %v", id, err)
		}
		return false
	}
	if job.Status != scheduler.StatusPending {
		return false
	}
	plan, reason := e.checkJob(job)
	if reason != "" {
		ok, err := e.store.FailConfig(ctx, id, e.epoch, reason, e.o.Now())
		if err != nil {
			e.logf("claim %s: %v", id, err)
			e.checkEpoch()
			return false
		}
		if ok {
			e.logf("job %s: config_error: %s", id, reason)
		}
		return ok
	}
	token, err := newClaimToken()
	if err != nil {
		e.logf("claim %s: token: %v", id, err)
		return false
	}
	gen, ok := e.fl.Insert(token, StageClaiming)
	if !ok {
		return false
	}
	c, err := e.store.Claim(ctx, scheduler.ClaimParams{JobID: id, Epoch: e.epoch, Now: e.o.Now(), Token: token})
	if err != nil {
		// An error other than a lost claim may be a commit whose answer was
		// lost: if the row carries our token, the claim stands.
		if !errors.Is(err, scheduler.ErrLostClaim) {
			if cl, cerr := e.store.ClaimFor(ctx, token, e.epoch); cerr == nil {
				c, err = cl, nil
			}
		}
		if err != nil {
			e.fl.Discard(token, gen)
			if !errors.Is(err, scheduler.ErrLostClaim) {
				e.logf("claim %s: %v", id, err)
				e.checkEpoch()
			}
			return false
		}
	}
	if c.AttemptNo != job.AttemptSeq+1 {
		plan.opts.OutputFile = outputFor(job.OutputBase, id, c.AttemptNo)
		plan.outputFile = plan.opts.OutputFile
	}
	e.fl.Advance(token, gen, StageLaunching)
	go e.launch(c, gen, plan)
	return true
}

// retry runs fn until it succeeds. It returns at once for an error that
// retrying cannot fix and when the runner is stopping.
func (e *engine) retry(kind string, fn func() error) error {
	delay := e.o.RetryBase
	for {
		err := e.inj(kind)
		if err == nil {
			err = fn()
		}
		if err == nil {
			return nil
		}
		if errors.Is(err, errNotOurs) || errors.Is(err, scheduler.ErrLostClaim) || errors.Is(err, scheduler.ErrCancelled) {
			return err
		}
		e.logf("%s write failed (will retry in %s): %v", kind, delay, err)
		select {
		case <-e.abort:
			return errAborted
		case <-time.After(delay):
		}
		if delay *= 2; delay > e.o.RetryMax {
			delay = e.o.RetryMax
		}
	}
}

// persist runs a write-once attempt-row write under retry. A zero-row result
// is re-read: if the value is already there (an earlier try committed and lost
// its answer) it is done, otherwise the row is not ours.
func (e *engine) persist(kind, token string, write func() (bool, error), done func(*scheduler.AttemptRow) bool) error {
	return e.retry(kind, func() error {
		ok, err := write()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		a, err := e.store.AttemptByToken(context.Background(), token)
		switch {
		case err == nil && done(a):
			return nil
		case err == nil || errors.Is(err, scheduler.ErrNotFound):
			return errNotOurs
		}
		return err
	})
}

// launch runs on its own goroutine for each claimed job: attempt paths, Spawn,
// the launch_result marker, and the hand-off to the waiter or to completing.
func (e *engine) launch(c *scheduler.Claim, gen uint64, plan *plannedLaunch) {
	ctx := context.Background()
	token := c.Token
	if err := e.persist("marker", token, func() (bool, error) {
		return e.store.SetAttemptPaths(ctx, c, plan.outputFile, "none", "")
	}, func(a *scheduler.AttemptRow) bool { return a.OutputFile == plan.outputFile }); err != nil {
		if errors.Is(err, errAborted) {
			return
		}
		e.lost(token)
		return
	}
	if h := e.o.hooks.beforeSpawn; h != nil {
		h(token)
	}

	opts := plan.opts
	opts.StrictExecutor = true
	opts.ExtraEnv = map[string]string{
		"IC_SCHEDULER_JOB_ID":  c.JobID,
		"IC_SCHEDULER_ATTEMPT": strconv.Itoa(c.AttemptNo),
	}
	opts.OnAdmitted = func(ctx context.Context, q dispatch.Querier, id string) error {
		return scheduler.LinkTx(ctx, q, c, id, e.o.Now())
	}
	res, serr := dispatch.Spawn(ctx, e.ds, opts)

	result := scheduler.LaunchStarted
	var rej *dispatch.SpawnRejection
	switch {
	case serr == nil:
	case errors.As(serr, &rej):
		result = scheduler.LaunchRejected
	case errors.Is(serr, scheduler.ErrLostClaim), errors.Is(serr, scheduler.ErrCancelled):
		result = scheduler.LaunchHookAborted
	default:
		result = scheduler.LaunchSpawnError
	}
	if serr != nil {
		e.logf("job %s: launch %s: %v", c.JobID, result, serr)
	}
	if err := e.persist("marker", token, func() (bool, error) {
		return e.store.RecordLaunchResult(ctx, c, result)
	}, func(a *scheduler.AttemptRow) bool { return a.LaunchResult == result }); err != nil {
		if res != nil {
			// The process runs but the row is not ours or we are stopping:
			// reap it so no zombie is left, and let recovery decide the rest.
			go func() { _ = res.Cmd.Wait() }()
			if !errors.Is(err, errAborted) {
				_ = dispatch.Kill(ctx, e.ds, res.ID)
			}
		}
		if !errors.Is(err, errAborted) {
			e.lost(token)
		}
		return
	}
	if h := e.o.hooks.afterLaunchPersist; h != nil {
		h(token, result)
	}
	if result != scheduler.LaunchStarted {
		e.fl.Advance(token, gen, StageCompleting)
		e.signal()
		return
	}

	// started: exactly one waiter owns the process from here, whatever
	// happens to the stage transition.
	if err := e.retry("marker", func() error { return e.store.MarkRunning(ctx, c, e.o.Now()) }); err != nil {
		switch {
		case errors.Is(err, scheduler.ErrCancelled):
			_ = dispatch.Kill(ctx, e.ds, res.ID)
		case errors.Is(err, scheduler.ErrLostClaim):
			e.checkEpoch()
		case errors.Is(err, errAborted):
		default:
			e.logf("job %s: mark running: %v", c.JobID, err)
		}
	}
	go e.wait(c, gen, res)
	e.fl.Advance(token, gen, StageWaiting)
}

// wait is the one waiter of a started process. The exit marker is written
// whether or not the entry is still ours, so the repair pass can see it.
func (e *engine) wait(c *scheduler.Claim, gen uint64, res *dispatch.SpawnResult) {
	exited := make(chan struct{})
	go e.reapTerminalDispatch(res, exited)
	_ = res.Cmd.Wait()
	close(exited)
	ctx := context.Background()
	err := e.persist("marker", c.Token, func() (bool, error) {
		return e.store.MarkExited(ctx, c, e.o.Now())
	}, func(a *scheduler.AttemptRow) bool { return a.ExitedAt != 0 })
	if err != nil {
		if !errors.Is(err, errAborted) {
			e.lost(c.Token)
		}
		return
	}
	if h := e.o.hooks.afterExitBeforeEvidence; h != nil {
		h(c.Token)
	}
	e.fl.Advance(c.Token, gen, StageCompleting)
	e.signal()
}

// reapTerminalDispatch ends a child whose dispatch row went terminal behind the
// runner's back (a manual collect or kill). The terminal row says the work is
// over, but a collect never signals the process, so the waiter would block on a
// live child for ever and hold its concurrency slot. The runner started this
// child and has not yet waited on it, so the identity captured at spawn is
// still its own and Terminate cannot hit a reused pid. It stops when the
// waiter sees the exit, or when the runner itself returns.
func (e *engine) reapTerminalDispatch(res *dispatch.SpawnResult, exited <-chan struct{}) {
	t := time.NewTicker(e.o.Tick)
	defer t.Stop()
	for {
		select {
		case <-exited:
			return
		case <-e.abort:
			// The runner is returning and closes its database. The child
			// is left to the next runner's recovery, as at a drain timeout.
			return
		case <-t.C:
		}
		d, err := e.ds.Get(context.Background(), res.ID)
		if err != nil || !d.IsTerminal() {
			continue
		}
		// The wait for a graceful exit can take seconds, so it also ends with
		// the runner: after abort the child has had its SIGTERM, gets no
		// SIGKILL from us, and is left to the next runner's recovery. Nothing
		// here touches the database, and nothing is logged once the runner is
		// returning.
		if err := res.TerminateUntil(e.abort); err != nil {
			if errors.Is(err, dispatch.ErrTerminationAbandoned) {
				return
			}
			select {
			case <-e.abort:
			default:
				e.logf("dispatch %s is terminal but its process was not stopped: %v", res.ID, err)
			}
		}
	}
}

// cancelStep signals the dispatch of every cancelling job this epoch owns. The
// waiter then sees the exit and the job completes as cancelled. Killing can
// wait seconds for a process that ignores SIGTERM, so each signal runs in its
// own goroutine: the main loop keeps heartbeating, completing and honouring
// the drain deadline meanwhile. At most one signal per token is in progress.
func (e *engine) cancelStep() {
	ctx := context.Background()
	cl, err := e.store.CancellingClaims(ctx, e.epoch)
	if err != nil {
		e.logf("cancel: %v", err)
		return
	}
	for _, c := range cl {
		e.mu.Lock()
		skip := e.killed[c.Token] || e.killing[c.Token]
		if !skip {
			e.killing[c.Token] = true
		}
		e.mu.Unlock()
		if skip {
			continue
		}
		a, err := e.store.AttemptByToken(ctx, c.Token)
		if err != nil || a.DispatchID == "" {
			e.clearKilling(c.Token, false)
			continue
		}
		go func(c scheduler.CancellingClaim, dispatchID string) {
			err := dispatch.Kill(context.Background(), e.ds, dispatchID)
			select {
			case <-e.abort: // the runner has returned; its database is closed
			default:
				if err != nil {
					e.logf("job %s: cancel: %v", c.JobID, err)
				}
			}
			e.clearKilling(c.Token, err == nil)
		}(c, a.DispatchID)
	}
}

func (e *engine) clearKilling(token string, signalled bool) {
	e.mu.Lock()
	delete(e.killing, token)
	if signalled {
		e.killed[token] = true
	}
	e.mu.Unlock()
}

// completeStep runs the completing stage of every due entry, in token order.
// A failed step schedules a retry with backoff and never gives up.
func (e *engine) completeStep() {
	for _, it := range e.fl.InStage(StageCompleting) {
		e.mu.Lock()
		b, waiting := e.backoff[it.Token]
		e.mu.Unlock()
		if waiting && time.Now().Before(b.next) {
			continue
		}
		if err := e.complete(it); err != nil {
			e.mu.Lock()
			d := b.delay
			if d == 0 {
				d = e.o.RetryBase
			} else if d *= 2; d > e.o.RetryMax {
				d = e.o.RetryMax
			}
			e.backoff[it.Token] = backoffState{next: time.Now().Add(d), delay: d}
			e.mu.Unlock()
			e.logf("completing %s: %v (retry in %s)", it.Token, err, d)
			continue
		}
		e.mu.Lock()
		delete(e.backoff, it.Token)
		delete(e.killed, it.Token)
		e.mu.Unlock()
	}
}

func (e *engine) stillOwned(it Item) bool {
	st, g, ok := e.fl.Stage(it.Token)
	return ok && g == it.Gen && st == StageCompleting
}

// complete runs the evidence pipeline for one entry: evidence (write-once),
// Collect, Classify, Finalize. It returns nil when the entry is gone or
// finalized, and an error to be retried otherwise.
func (e *engine) complete(it Item) error {
	ctx := context.Background()
	c, err := e.store.ClaimFor(ctx, it.Token, e.epoch)
	if err != nil {
		if errors.Is(err, scheduler.ErrLostClaim) || errors.Is(err, scheduler.ErrNotFound) {
			// finalized elsewhere (cancel/recovery) or another epoch's row
			e.checkEpoch()
			e.fl.Remove(it.Token, it.Gen)
			return nil
		}
		return err
	}
	a, err := e.store.AttemptByToken(ctx, it.Token)
	if err != nil {
		return err
	}

	if a.EvidenceClass == "" {
		if err := e.inj("evidence"); err != nil {
			return err
		}
		class, sha, source := readReceipt(a)
		ok, err := e.store.WriteEvidence(ctx, c, class, sha, source)
		if err != nil {
			return err
		}
		if !ok {
			// write-once: another writer (or an earlier try) already set it
			if a, err = e.store.AttemptByToken(ctx, it.Token); err != nil {
				return err
			}
			if a.EvidenceClass == "" {
				e.lost(it.Token)
				return nil
			}
		}
	}
	if h := e.o.hooks.afterEvidenceBeforeCollect; h != nil {
		h(it.Token)
	}
	if !e.stillOwned(it) {
		return nil
	}

	if a.DispatchID != "" {
		if err := e.inj("collect"); err != nil {
			return err
		}
		if err := dispatch.Collect(ctx, e.ds, a.DispatchID); err != nil {
			return fmt.Errorf("collect %s: %w", a.DispatchID, err)
		}
	}
	// Read the row again after Collect: the cancel flag and the evidence are
	// what Classify needs, and both can change while we collect.
	if a, err = e.store.AttemptByToken(ctx, it.Token); err != nil {
		return err
	}
	ev := Evidence{
		Cancelling:    a.JobStatus == scheduler.StatusCancelling || a.JobStatus == scheduler.StatusCancelled,
		EvidenceClass: a.EvidenceClass,
	}
	if a.DispatchID != "" {
		d, err := e.ds.Get(ctx, a.DispatchID)
		switch {
		case err == nil:
			ev.DispatchPresent = true
			ev.Status = d.Status
			if d.VerdictStatus != nil {
				ev.Verdict = *d.VerdictStatus
			}
			var fc sql.NullString
			if err := e.sqlDB.QueryRowContext(ctx,
				`SELECT failure_class FROM dispatch_terminals WHERE dispatch_id = ?`, a.DispatchID).Scan(&fc); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			ev.FailureClass = fc.String
		case errors.Is(err, dispatch.ErrNotFound):
		default:
			return err
		}
	}
	class := Classify(ev)
	if a.DispatchID == "" && !ev.Cancelling {
		switch a.LaunchResult {
		case scheduler.LaunchRejected, scheduler.LaunchHookAborted, scheduler.LaunchSpawnError:
			// nothing ever started: a counted pre-start failure
			class = scheduler.ClassPrestart
		}
	}

	fp := scheduler.FinalizeParams{Class: class, Now: e.o.Now()}
	if class != scheduler.ClassOK {
		fp.Error = fmt.Sprintf("attempt ended %s (launch_result=%s)", class, a.LaunchResult)
	}
	if a.LaunchResult == scheduler.LaunchStarted {
		// Without containment a started job may have left descendants: a
		// post-start retry is blocked until someone confirms none survive.
		job, err := e.store.Get(ctx, a.JobID)
		if err != nil {
			return err
		}
		pol, perr := scheduler.ParseRetryPolicy(job.RetryPolicy, job.Idempotent)
		if perr == nil && scheduler.ShouldRetry(pol, class, job.AttemptSeq, job.CrashRequeues, false) {
			fp.RetryBlocked = "survivor_unknown:" + a.DispatchID
		}
	}
	if !e.stillOwned(it) {
		return nil
	}
	if err := e.inj("finalize"); err != nil {
		return err
	}
	if _, err := e.store.Finalize(ctx, c, fp); err != nil {
		if errors.Is(err, scheduler.ErrLostClaim) {
			e.checkEpoch()
			e.fl.Remove(it.Token, it.Gen)
			return nil
		}
		return err
	}
	e.fl.Remove(it.Token, it.Gen)
	return nil
}

// repairStep cross-checks the map against the database (plan section 5.6).
func (e *engine) repairStep() {
	ctx := context.Background()
	row := func(token string) (RepairRow, error) {
		a, err := e.store.AttemptByToken(ctx, token)
		if errors.Is(err, scheduler.ErrNotFound) {
			return RepairRow{Token: token}, nil
		}
		if err != nil {
			return RepairRow{}, err
		}
		r := RepairRow{Token: token, LaunchResult: a.LaunchResult, Exited: a.ExitedAt != 0}
		if _, err := e.store.ClaimFor(ctx, token, e.epoch); err == nil {
			r.InFlight = true
		} else if !errors.Is(err, scheduler.ErrLostClaim) && !errors.Is(err, scheduler.ErrNotFound) {
			return RepairRow{}, err
		}
		return r, nil
	}
	n, err := e.fl.Repair(RepairDeps{
		Rows: func() ([]RepairRow, error) {
			cl, err := e.store.InFlightClaims(ctx, e.epoch)
			if err != nil {
				return nil, err
			}
			out := make([]RepairRow, 0, len(cl))
			for _, c := range cl {
				r, err := row(c.Token)
				if err != nil {
					return nil, err
				}
				r.InFlight = true
				out = append(out, r)
			}
			return out, nil
		},
		Reread: row,
		FailStop: func(token, stage string, r RepairRow) {
			if h := e.o.hooks.failStop; h != nil {
				h(token, stage)
				return
			}
			fmt.Fprintf(e.o.Stderr, "ic scheduler run: invariant violated: in-flight row %s has no map entry and is not completion-ready (stage %s, launch_result=%q, exited=%v); exiting\n",
				token, stage, r.LaunchResult, r.Exited)
			os.Exit(ExitInvariant)
		},
	})
	if err != nil {
		e.logf("repair: %v", err)
	}
	if n > 0 {
		e.logf("repair: re-inserted %d completing entr(ies)", n)
		e.signal()
	}
}
