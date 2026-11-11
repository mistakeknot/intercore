//go:build icsched_test && linux

package runner

import (
	"context"
	"errors"
	"fmt"

	"golang.org/x/sys/unix"

	"github.com/mistakeknot/intercore/internal/dispatch"
	"github.com/mistakeknot/intercore/internal/scheduler"
)

// The legacy rule (plan section 3 and 5.3d, row 1). A legacy=1 attempt is the
// synthetic one migration 041 made for a row the pre-runner executor left in
// flight. It is never adopted and never re-run. A process that is alive, or
// that cannot be proven gone, is left alone and the job is quarantined
// legacy_running. Otherwise the attempt goes through the evidence pipeline and
// is finalized under the capped legacy rule (cas.go recoverFinalize): ok is
// completed, another definite class is failed with that class, and
// indeterminate, unknown or a pruned dispatch is quarantined legacy_unknown.

// recoverLegacy applies the legacy rule to one prior-epoch legacy attempt.
func (e *engine) recoverLegacy(ctx context.Context, c *scheduler.Claim, a *scheduler.AttemptRow, d *dispatch.Dispatch, state scheduler.DispatchState) error {
	alive, why := e.legacyProcessAlive(ctx, a, d, state)
	if alive {
		e.logf("recovery: job %s is a legacy attempt with a process that may be alive (%s): left alone, quarantined %s", a.JobID, why, scheduler.ReasonLegacyRunning)
		e.recoverStep(a.Token, "legacy_running")
		return e.quarantineLegacyRunning(ctx, a)
	}
	e.recoverStep(a.Token, "legacy_pipeline")
	return e.recoveredPipeline(ctx, a, e.pipeline(ctx, c, a, nil, true))
}

// legacyProcessAlive reports whether the legacy attempt's worker is alive or
// cannot be proven gone. It only looks: no signal is ever sent to a legacy
// process. A terminal or pruned dispatch has no live worker by definition.
func (e *engine) legacyProcessAlive(ctx context.Context, a *scheduler.AttemptRow, d *dispatch.Dispatch, state scheduler.DispatchState) (bool, string) {
	switch state {
	case scheduler.DispLivePID:
		id, why := readRecordedIdentity(ctx, e.sqlDB, d)
		if id == nil {
			// Older dispatches carry a PID with no recorded identity: nothing
			// ties the PID to this job, and nothing proves it gone.
			if e.rebooted() {
				return false, ""
			}
			return true, "recorded PID with no identity: " + why
		}
		v := e.checkRecorded(id)
		if v.PidFD >= 0 {
			_ = unix.Close(v.PidFD) // legacy processes are never adopted
		}
		switch v.State {
		case pidZombie, pidDead:
			return false, ""
		}
		return true, "recorded process " + v.State.String()
	case scheduler.DispLiveNoPID:
		if e.rebooted() {
			return false, ""
		}
		out := e.sweepEnvironment(a.DispatchID, false)
		if out.Alive() {
			return true, fmt.Sprintf("environment sweep: %d found", out.Found)
		}
	}
	return false, ""
}

// quarantineLegacyRunning parks the job as legacy_running. A job that is
// cancelling stays in flight: the worker may still be running and the cancel
// is for the operator's force path.
func (e *engine) quarantineLegacyRunning(ctx context.Context, a *scheduler.AttemptRow) error {
	_, err := e.store.Recover(ctx, scheduler.RecoverParams{
		JobID: a.JobID, Epoch: e.epoch, Action: scheduler.ActQuarantine, Reason: scheduler.ReasonLegacyRunning,
		Now: e.o.Now(), KeepCancelling: true,
	})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, scheduler.ErrLostClaim):
		e.checkEpoch()
		return nil
	case errors.Is(err, scheduler.ErrCancelPending):
		e.logf("recovery: job %s legacy process may be alive and a cancel is pending; left cancelling", a.JobID)
		return nil
	}
	return err
}
