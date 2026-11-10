//go:build icsched_test && linux

package runner

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mistakeknot/intercore/internal/dispatch"
	"github.com/mistakeknot/intercore/internal/scheduler"
)

// The prior-epoch recovery pass (plan section 5.3d). It runs once before the
// first claim and then every RecoverEvery. Its scope is the in-flight rows of
// an older epoch; it touches only dispatches linked from
// scheduler_job_attempts. Slice 4b implements the table rows that need no
// process lookup: 2 to 6, the cgroup-mode half of 8, row 9, and the boot
// change shortcut. Row 7 (a recorded PID), the legacy row and the fallback
// mode survivor handling are skipped with a log line and left to slices 4c and
// 4d; a skipped row stays in flight and is looked at again by the next pass.

// recoveryPass applies the recovery table to every prior-epoch row.
func (e *engine) recoveryPass() {
	ctx := context.Background()
	claims, err := e.store.PriorEpochClaims(ctx, e.epoch)
	if err != nil {
		e.logf("recovery: list prior-epoch rows: %v", err)
	}
	for _, pc := range claims {
		e.mu.Lock()
		b, waiting := e.backoff[pc.Token]
		e.mu.Unlock()
		if waiting && time.Now().Before(b.next) {
			continue
		}
		if err := e.recoverRow(ctx, pc); err != nil {
			e.mu.Lock()
			d := b.delay
			if d == 0 {
				d = e.o.RetryBase
			} else if d *= 2; d > e.o.RetryMax {
				d = e.o.RetryMax
			}
			e.backoff[pc.Token] = backoffState{next: time.Now().Add(d), delay: d}
			e.mu.Unlock()
			e.logf("recovery: job %s (%s): %v (retry in %s)", pc.JobID, pc.Token, err, d)
			continue
		}
		e.mu.Lock()
		delete(e.backoff, pc.Token)
		e.mu.Unlock()
	}
	if h := e.o.hooks.recoveryPass; h != nil {
		h()
	}
}

func (e *engine) recoverStep(token, step string) {
	if h := e.o.hooks.recoverRow; h != nil {
		h(token, step)
	}
}

// recoveryFacts reads the dispatch of an attempt and reduces it to the state
// DecideRecovery matches on. The returned dispatch is nil unless it exists.
func (e *engine) recoveryFacts(ctx context.Context, a *scheduler.AttemptRow) (scheduler.DispatchState, *dispatch.Dispatch, error) {
	if a.DispatchID == "" {
		return scheduler.DispAbsent, nil, nil
	}
	d, err := e.ds.Get(ctx, a.DispatchID)
	switch {
	case errors.Is(err, dispatch.ErrNotFound):
		return scheduler.DispPruned, nil, nil
	case err != nil:
		return 0, nil, err
	case d.IsTerminal():
		return scheduler.DispTerminal, d, nil
	case d.PID != nil:
		return scheduler.DispLivePID, d, nil
	}
	return scheduler.DispLiveNoPID, d, nil
}

func (e *engine) rebooted() bool {
	return e.priorBoot != "" && e.priorBoot != e.o.bootID()
}

func containedAttempt(a *scheduler.AttemptRow) bool {
	return a.Containment == "cgroup" && a.CgroupPath != ""
}

// recoverRow applies one row of the table to one prior-epoch claim. A nil
// return means the row is finished or deliberately left for a later slice; an
// error leaves it in flight for the next pass.
func (e *engine) recoverRow(ctx context.Context, pc scheduler.PriorClaim) error {
	c, err := e.store.PriorClaimFor(ctx, pc.Token, e.epoch)
	if err != nil {
		if errors.Is(err, scheduler.ErrLostClaim) || errors.Is(err, scheduler.ErrNotFound) {
			return nil // finalized or re-claimed since the list was read
		}
		return err
	}
	a, err := e.store.AttemptByToken(ctx, pc.Token)
	if err != nil {
		if errors.Is(err, scheduler.ErrNotFound) {
			return nil
		}
		return err
	}
	state, _, err := e.recoveryFacts(ctx, a)
	if err != nil {
		return err
	}
	dec := scheduler.DecideRecovery(scheduler.RecoveryInput{
		Status:       a.JobStatus,
		Legacy:       a.Legacy,
		Linked:       a.LinkedAt != 0 || a.DispatchID != "",
		LaunchResult: a.LaunchResult,
		Dispatch:     state,
	})
	e.recoverStep(pc.Token, fmt.Sprintf("row%d:%s", dec.Row, dec.Action))

	switch dec.Action {
	case scheduler.ActLegacy:
		e.logf("recovery: job %s is a legacy attempt (row %d); the legacy rule is not part of this runner yet, left as is", a.JobID, dec.Row)
		return nil
	case scheduler.ActCheckPID, scheduler.ActSurvivor:
		if e.rebooted() {
			return e.recoverRebooted(ctx, c, a)
		}
		if dec.Action == scheduler.ActCheckPID {
			e.logf("recovery: job %s has a recorded PID (row %d); the recorded-PID check is not part of this runner yet, left as is", a.JobID, dec.Row)
			return nil
		}
		if !containedAttempt(a) {
			e.logf("recovery: job %s has no recorded PID and no attempt cgroup (row %d); the fallback-mode survivor sweep is not part of this runner yet, left as is", a.JobID, dec.Row)
			return nil
		}
		return e.recoverSurvivor(ctx, c, a)
	case scheduler.ActPipeline:
		return e.recoverPipeline(ctx, c, a)
	}
	return e.recoverUnlinked(ctx, c, a, dec)
}

// recoverPipeline is row 6: the dispatch is terminal or pruned, so the shared
// pipeline runs (it skips Collect) and the job is finalized. A pruned dispatch
// classifies indeterminate.
func (e *engine) recoverPipeline(ctx context.Context, c *scheduler.Claim, a *scheduler.AttemptRow) error {
	err := e.pipeline(ctx, c, a, nil, e.rebooted())
	return e.recoveredPipeline(ctx, a, err)
}

// recoveredPipeline turns the pipeline's outcome into the row's outcome.
func (e *engine) recoveredPipeline(ctx context.Context, a *scheduler.AttemptRow, err error) error {
	switch {
	case err == nil:
		e.recoverStep(a.Token, "finalized")
		return nil
	case errors.Is(err, errPipelineLost), errors.Is(err, scheduler.ErrLostClaim):
		e.checkEpoch()
		return nil // another writer finished the row first
	case errors.Is(err, errCgroupPath):
		return e.quarantineRow(ctx, a, scheduler.ReasonRunnerInconsistent, err)
	}
	return err
}

// recoverRebooted is the boot change shortcut: the previous runner ran on an
// earlier boot, so none of its processes exists. A non-terminal dispatch is
// terminalized by reconcile with no process lookup and the pipeline finishes
// the job as indeterminate.
func (e *engine) recoverRebooted(ctx context.Context, c *scheduler.Claim, a *scheduler.AttemptRow) error {
	if err := e.terminalizeIndeterminate(ctx, a, "reboot", 0); err != nil {
		return err
	}
	e.recoverStep(a.Token, "terminalized")
	return e.recoveredPipeline(ctx, a, e.pipeline(ctx, c, a, nil, true))
}

// recoverSurvivor is row 8 in cgroup mode. Every process of the attempt is in
// its cgroup, so the cgroup is killed and proven empty and the dispatch is
// terminalized indeterminate, with orphans_killed set when cgroup.procs held
// anything. No environment scan, no adoption, and no retry_blocked: the cgroup
// proof is the proof.
func (e *engine) recoverSurvivor(ctx context.Context, c *scheduler.Claim, a *scheduler.AttemptRow) error {
	n, err := e.extinguish(ctx, c, a)
	if err != nil {
		if errors.Is(err, errCgroupPath) {
			return e.quarantineRow(ctx, a, scheduler.ReasonRunnerInconsistent, err)
		}
		return err
	}
	e.recoverStep(a.Token, "extinct")
	if err := e.terminalizeIndeterminate(ctx, a, "survivor", n); err != nil {
		return err
	}
	e.recoverStep(a.Token, "terminalized")
	return e.recoveredPipeline(ctx, a, e.pipeline(ctx, c, a, nil, true))
}

// terminalizeIndeterminate closes a non-terminal dispatch as an indeterminate
// failure written by reconcile. An already-terminal or pruned dispatch is left
// alone: the pipeline reads whatever is there.
func (e *engine) terminalizeIndeterminate(ctx context.Context, a *scheduler.AttemptRow, reason string, orphans int) error {
	now := e.o.Now().Unix()
	_, err := e.ds.Terminalize(ctx, a.DispatchID, dispatch.Terminal{
		Status:        dispatch.StatusFailed,
		Source:        dispatch.TerminalSourceReconcile,
		Evidence:      dispatch.EvidenceNotApplicable,
		Reason:        reason,
		FailureClass:  dispatch.WorkerOutcomeIndeterminate,
		OrphansKilled: orphans,
		Fields: dispatch.UpdateFields{
			"completed_at":      now,
			"error_message":     "recovered by a new runner epoch: " + reason,
			"quarantine_reason": dispatch.WorkerOutcomeIndeterminate,
		},
	})
	switch {
	case err == nil, errors.Is(err, dispatch.ErrAlreadyTerminal), errors.Is(err, dispatch.ErrNotFound):
		return nil
	}
	return fmt.Errorf("terminalize dispatch %s: %w", a.DispatchID, err)
}

// recoverUnlinked applies rows 2 to 5 and the catch-all row 9: attempts that
// never reached a dispatch, and tuples that cannot happen. Any recorded
// attempt cgroup is killed and proven empty first.
func (e *engine) recoverUnlinked(ctx context.Context, c *scheduler.Claim, a *scheduler.AttemptRow, dec scheduler.RecoveryDecision) error {
	if _, err := e.extinguish(ctx, c, a); err != nil {
		if errors.Is(err, errCgroupPath) {
			return e.quarantineRow(ctx, a, scheduler.ReasonRunnerInconsistent, err)
		}
		return err
	}
	e.recoverStep(a.Token, "extinct")
	p := scheduler.RecoverParams{
		JobID:  a.JobID,
		Epoch:  e.epoch,
		Action: dec.Action,
		Now:    e.o.Now(),
	}
	switch dec.Action {
	case scheduler.ActFinalize:
		p.Class = dec.Reason
	case scheduler.ActQuarantine:
		p.Reason = dec.Reason
	}
	if _, err := e.store.Recover(ctx, p); err != nil {
		if errors.Is(err, scheduler.ErrLostClaim) {
			e.checkEpoch()
			return nil
		}
		return err
	}
	e.recoverStep(a.Token, "recovered")
	e.removeAttemptCgroup(a)
	return nil
}

// quarantineRow parks a row that must not be touched further.
func (e *engine) quarantineRow(ctx context.Context, a *scheduler.AttemptRow, reason string, cause error) error {
	e.logf("recovery: job %s quarantined %s: %v", a.JobID, reason, cause)
	_, err := e.store.Recover(ctx, scheduler.RecoverParams{
		JobID: a.JobID, Epoch: e.epoch, Action: scheduler.ActQuarantine, Reason: reason, Now: e.o.Now(),
	})
	if errors.Is(err, scheduler.ErrLostClaim) {
		e.checkEpoch()
		return nil
	}
	return err
}

// removeAttemptCgroup removes a proven-empty attempt directory.
func (e *engine) removeAttemptCgroup(a *scheduler.AttemptRow) {
	if !containedAttempt(a) || e.cg == nil {
		return
	}
	if err := e.cg.remove(a.CgroupPath); err != nil {
		e.logf("job %s: remove attempt cgroup %s: %v", a.JobID, a.CgroupPath, err)
	}
}
