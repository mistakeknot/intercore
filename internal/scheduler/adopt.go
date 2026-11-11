package scheduler

import (
	"context"
	"errors"
	"fmt"

	"github.com/mistakeknot/intercore/internal/db"
)

// Adopt hands the in-flight attempt of an older runner epoch to the runner at
// epoch p.Epoch: one guarded transaction replaces the claim token and the lease
// epoch of both the job row and its open attempt row. It is how a restarted
// runner takes over a worker that is provably still the process the previous
// runner started (plan section 5.3d, row 7).
//
// The new token is the only proof of ownership from then on, so the previous
// runner (fenced by the epoch already) can never write through the old one. The
// write fails with ErrLostClaim unless, inside the transaction, the epoch is
// current, the row is in recovery scope and still carries OldToken.
func (s *Store) Adopt(ctx context.Context, jobID, oldToken, newToken string, epoch int64) (*Claim, error) {
	if oldToken == "" || newToken == "" || oldToken == newToken {
		return nil, fmt.Errorf("%w: adopt needs two distinct tokens", ErrBadRecovery)
	}
	var c *Claim
	err := s.inTx(ctx, func(tx *db.ImmediateTx) error {
		st, err := loadState(ctx, tx, jobID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return ErrLostClaim
			}
			return err
		}
		if st.runnerEpoch != epoch || !InRecoveryScope(st.status, st.lease, epoch) || st.token != oldToken {
			return ErrLostClaim
		}
		var attemptNo int
		if err := tx.QueryRowContext(ctx, `SELECT attempt_no FROM scheduler_job_attempts
			WHERE claim_token = ? AND job_id = ? AND ended_at IS NULL`, oldToken, jobID).Scan(&attemptNo); err != nil {
			return ErrLostClaim
		}
		res, err := tx.ExecContext(ctx, `UPDATE scheduler_jobs SET claim_token = ?, lease_epoch = ?
			WHERE id = ? AND claim_token = ? AND status IN `+inFlightSQL+` AND `+epochGuard,
			newToken, epoch, jobID, oldToken, epoch)
		if err != nil {
			return fmt.Errorf("adopt %s: job: %w", jobID, err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrLostClaim
		}
		res, err = tx.ExecContext(ctx, `UPDATE scheduler_job_attempts SET claim_token = ?, lease_epoch = ?
			WHERE claim_token = ? AND job_id = ? AND ended_at IS NULL`, newToken, epoch, oldToken, jobID)
		if err != nil {
			return fmt.Errorf("adopt %s: attempt: %w", jobID, err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrLostClaim
		}
		c = &Claim{JobID: jobID, Token: newToken, AttemptNo: attemptNo, Epoch: epoch}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return c, nil
}
