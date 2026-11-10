//go:build icsched_test && linux

package runner

import (
	"context"
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/unix"

	"github.com/mistakeknot/intercore/internal/dispatch"
	"github.com/mistakeknot/intercore/internal/scheduler"
)

// unreadableBound is how long a prior row whose process cannot be read (or
// whose identity cannot be proven) is re-checked before the runner stops
// waiting for it (plan section 5.3d).
const unreadableBound = 5 * time.Minute

func (e *engine) clockNow() time.Time {
	if h := e.o.hooks.clock; h != nil {
		return h()
	}
	return e.o.Now()
}

// recoverRecordedPID is row 7: the dispatch is live and has a recorded PID.
func (e *engine) recoverRecordedPID(ctx context.Context, c *scheduler.Claim, a *scheduler.AttemptRow, d *dispatch.Dispatch) error {
	var v pidVerdict
	id, why := readRecordedIdentity(ctx, e.sqlDB, d)
	if id == nil {
		v = pidVerdict{State: pidProofAbsent, Detail: why, PidFD: -1}
	} else {
		v = e.checkRecorded(id)
	}
	e.recoverStep(a.Token, "pid:"+v.State.String())
	e.logf("recovery: job %s recorded process: %s (%s)", a.JobID, v.State, v.Detail)

	switch v.State {
	case pidAliveMatch:
		e.clearUnreadable(a.Token)
		return e.adopt(ctx, c, a, v)
	case pidZombie, pidDead:
		// Gone. With no receipt the pipeline classifies indeterminate; in
		// fallback mode a started job that would retry is blocked as
		// survivor_unknown unless it is proven otherwise.
		e.clearUnreadable(a.Token)
		return e.recoveredPipeline(ctx, a, e.pipeline(ctx, c, a, nil, false))
	}
	return e.recoverUnreadable(ctx, c, a, v)
}

func (e *engine) clearUnreadable(token string) {
	e.mu.Lock()
	delete(e.unread, token)
	e.mu.Unlock()
}

// recoverUnreadable handles unreadable, proof_absent and pgid_changed: the row
// is re-checked on every pass; past five minutes cgroup mode kills the attempt
// cgroup, proves it empty and finishes as indeterminate, and fallback mode
// quarantines survivor_unknown and signals nothing. The timer is in memory, so
// a restarted runner starts it again; that only delays the verdict.
func (e *engine) recoverUnreadable(ctx context.Context, c *scheduler.Claim, a *scheduler.AttemptRow, v pidVerdict) error {
	now := e.clockNow()
	e.mu.Lock()
	first, seen := e.unread[a.Token]
	if !seen {
		e.unread[a.Token] = now
		first = now
	}
	e.mu.Unlock()
	if now.Sub(first) <= unreadableBound {
		return nil // looked at again by the next pass
	}
	e.clearUnreadable(a.Token)
	if containedAttempt(a) && e.cg != nil {
		e.logf("recovery: job %s process unreadable for over %s (%s): killing its cgroup", a.JobID, unreadableBound, v.State)
		return e.recoverSurvivor(ctx, c, a)
	}
	return e.quarantineRow(ctx, a, scheduler.ReasonSurvivorUnknown, fmt.Errorf("recorded process %s for over %s: %s", v.State, unreadableBound, v.Detail))
}

// adopt takes over a worker that is provably the process the previous runner
// started. The new claim token and this epoch are written in one guarded
// transaction, the pidfd is kept, and a watcher waits for the exit and hands
// the row to the normal completion path.
func (e *engine) adopt(ctx context.Context, c *scheduler.Claim, a *scheduler.AttemptRow, v pidVerdict) error {
	closeFD := func() {
		if v.PidFD >= 0 {
			_ = unix.Close(v.PidFD)
		}
	}
	tok, err := newClaimToken()
	if err != nil {
		closeFD()
		return err
	}
	gen, ok := e.fl.Insert(tok, StageClaiming)
	if !ok {
		closeFD()
		return nil // at capacity: the next pass tries again
	}
	nc, err := e.store.Adopt(ctx, a.JobID, c.Token, tok, e.epoch)
	if err != nil && !errors.Is(err, scheduler.ErrLostClaim) {
		// The commit may have succeeded and the answer been lost.
		if cl, cerr := e.store.ClaimFor(ctx, tok, e.epoch); cerr == nil {
			nc, err = cl, nil
		}
	}
	if err != nil {
		e.fl.Discard(tok, gen)
		closeFD()
		if errors.Is(err, scheduler.ErrLostClaim) {
			e.checkEpoch()
			return nil
		}
		return err
	}
	e.recoverStep(tok, "adopted")
	e.logf("recovery: job %s adopted (pid of dispatch %s)", a.JobID, a.DispatchID)
	e.fl.Advance(tok, gen, StageLaunching)
	e.fl.Advance(tok, gen, StageWaiting)
	// A job that is cancelling is killed by the cancel step through the
	// recorded identity, and the pipeline then proves the cgroup empty.
	if !e.track(func() { e.watchAdopted(nc, gen, v.PidFD) }) {
		closeFD()
	}
	return nil
}

// procGone reports whether the adopted process of a fixture procfs has exited
// (gone, or a zombie). It reads the identity again each time.
func (e *engine) procGone(c *scheduler.Claim) bool {
	a, err := e.store.AttemptByToken(context.Background(), c.Token)
	if err != nil || a.DispatchID == "" {
		return false
	}
	d, err := e.ds.Get(context.Background(), a.DispatchID)
	if err != nil {
		return false
	}
	id, _ := readRecordedIdentity(context.Background(), e.sqlDB, d)
	if id == nil {
		return false
	}
	v := e.checkRecorded(id)
	if v.PidFD >= 0 {
		_ = unix.Close(v.PidFD)
	}
	return v.State == pidDead || v.State == pidZombie
}

// watchAdopted waits for the adopted process to exit. A pidfd turns readable at
// exit, also for a process that nobody reaps. With a fixture procfs there is no
// pidfd and the process cannot be watched, so the watcher only polls the stat.
func (e *engine) watchAdopted(c *scheduler.Claim, gen uint64, fd int) {
	if fd >= 0 {
		defer unix.Close(fd)
	}
	for {
		select {
		case <-e.abort:
			return
		default:
		}
		if fd < 0 {
			// A fixture procfs gives no pidfd: poll the stat instead.
			if !e.procGone(c) {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			break
		}
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, 200)
		if err != nil && !errors.Is(err, unix.EINTR) {
			e.logf("job %s: poll adopted process: %v", c.JobID, err)
			e.lost(c.Token)
			return
		}
		if n > 0 {
			break
		}
	}
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
	e.barrier("after_exit_before_evidence", c.Token)
	e.fl.Advance(c.Token, gen, StageCompleting)
	e.signal()
}
