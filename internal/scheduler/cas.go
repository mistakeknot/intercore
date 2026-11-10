package scheduler

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/mistakeknot/intercore/internal/db"
)

// Compare-and-swap transactions on the scheduler tables (plan sections 4 and 5).
//
// Every runner write carries the epoch guard, so a runner that lost the queue
// to a newer one writes nothing, and every write to an in-flight row also
// carries the claim token. A write that matches no row is re-read to say
// whether the job was cancelled under the runner (ErrCancelled) or the claim is
// simply gone (ErrLostClaim). The guard covers scheduler tables only.

var (
	// ErrLostClaim: the epoch, the token or the status no longer matches.
	ErrLostClaim = errors.New("scheduler: claim lost")
	// ErrCancelled: the job is cancelling; the runner must stop and finalize.
	ErrCancelled = errors.New("scheduler: job cancelled")
	// ErrAlreadyTerminal: the job already ended.
	ErrAlreadyTerminal = errors.New("scheduler: job already terminal")
	// ErrNotQuarantined: requeue applies to quarantined jobs only.
	ErrNotQuarantined = errors.New("scheduler: job is not quarantined")
	// ErrNotBlocked: the job has no retry_blocked marker.
	ErrNotBlocked = errors.New("scheduler: job is not retry-blocked")
	// ErrConfirmRequired: the operator did not pass --confirm-not-running.
	ErrConfirmRequired = errors.New("scheduler: --confirm-not-running is required")
	// ErrNotFound: no job with that ID.
	ErrNotFound = errors.New("scheduler: job not found")
	// ErrBadRecovery: the action cannot be applied by Recover.
	ErrBadRecovery = errors.New("scheduler: invalid recovery action")
)

// Claim is the runner's proof of ownership of one attempt.
type Claim struct {
	JobID     string
	Token     string
	AttemptNo int
	Epoch     int64
}

// ClaimParams selects the job to claim for a runner at Epoch.
type ClaimParams struct {
	JobID string
	Epoch int64
	Now   time.Time
}

// FinalizeParams ends the attempt of a claim.
type FinalizeParams struct {
	Class        string
	Now          time.Time
	Error        string
	RetryBlocked string         // set when a survivor may still be running
	Rand         func() float64 // jitter source; nil uses math/rand
}

// FinalizeResult is what a finalize or recover left the job as.
type FinalizeResult struct {
	Status    JobStatus
	Retry     bool
	NotBefore time.Time // set when Retry
}

// RecoverParams applies one decision of DecideRecovery to an orphaned row.
type RecoverParams struct {
	JobID        string
	Epoch        int64 // the recovering runner's epoch
	Action       RecoveryAction
	Class        string // ActFinalize: the outcome class
	Reason       string // ActQuarantine: the reason
	RetryBlocked string
	Now          time.Time
	Rand         func() float64
}

// CancelResult reports what CancelJob did.
type CancelResult struct {
	Status   JobStatus
	InFlight bool // the job was in flight, so a runner must observe the cancel
}

// execer is satisfied by *sql.Tx and *db.ImmediateTx.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

const epochGuard = `(SELECT epoch FROM scheduler_runner WHERE id = 1) = ?`

// inFlight lists the statuses in which a claim token is live.
const inFlightSQL = `('launching','running','cancelling')`

func (s *Store) inTx(ctx context.Context, fn func(tx *db.ImmediateTx) error) error {
	tx, err := db.BeginImmediate(ctx, s.db)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func newToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("claim token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// Claim moves a pending job to launching for the runner at p.Epoch. It refuses
// a job that is retry-blocked, not yet due, or queued while the scheduler is
// paused, and it refuses a runner whose epoch is no longer current.
func (s *Store) Claim(ctx context.Context, p ClaimParams) (*Claim, error) {
	token, err := newToken()
	if err != nil {
		return nil, err
	}
	now := p.Now.Unix()
	var c *Claim
	err = s.inTx(ctx, func(tx *db.ImmediateTx) error {
		res, err := tx.ExecContext(ctx, `UPDATE scheduler_jobs
			SET status = 'launching', attempt_seq = attempt_seq + 1, claim_token = ?, lease_epoch = ?, updated_at = ?
			WHERE id = ? AND status = 'pending' AND retry_blocked IS NULL AND not_before <= ?
			  AND `+epochGuard+`
			  AND COALESCE((SELECT payload FROM state WHERE key = 'scheduler_paused' AND scope_id = 'global'), 'false') <> 'true'`,
			token, p.Epoch, now, p.JobID, now, p.Epoch)
		if err != nil {
			return fmt.Errorf("claim %s: %w", p.JobID, err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrLostClaim
		}
		var seq int
		if err := tx.QueryRowContext(ctx, `SELECT attempt_seq FROM scheduler_jobs WHERE id = ?`, p.JobID).Scan(&seq); err != nil {
			return fmt.Errorf("claim %s: read attempt: %w", p.JobID, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO scheduler_job_attempts
			(job_id, attempt_no, claim_token, lease_epoch, claimed_at, counts_toward_limit)
			VALUES (?, ?, ?, ?, ?, 1)`, p.JobID, seq, token, p.Epoch, now); err != nil {
			return fmt.Errorf("claim %s: record attempt: %w", p.JobID, err)
		}
		c = &Claim{JobID: p.JobID, Token: token, AttemptNo: seq, Epoch: p.Epoch}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return c, nil
}

// diagnose says why a guarded write to an in-flight row matched nothing.
func diagnose(ctx context.Context, tx execer, c *Claim) error {
	var status, token string
	err := tx.QueryRowContext(ctx, `SELECT status, COALESCE(claim_token, '') FROM scheduler_jobs WHERE id = ?`, c.JobID).Scan(&status, &token)
	if err == nil && status == string(StatusCancelling) && token == c.Token {
		return ErrCancelled
	}
	return ErrLostClaim
}

// LinkTx records the dispatch on the job and the attempt inside the caller's
// transaction, so the link commits or rolls back with the dispatch admission.
func LinkTx(ctx context.Context, tx execer, c *Claim, dispatchID string, now time.Time) error {
	res, err := tx.ExecContext(ctx, `UPDATE scheduler_jobs
		SET dispatch_id = ?, updated_at = ?
		WHERE id = ? AND claim_token = ? AND status = 'launching' AND dispatch_id IS NULL AND `+epochGuard,
		dispatchID, now.Unix(), c.JobID, c.Token, c.Epoch)
	if err != nil {
		return fmt.Errorf("link %s: %w", c.JobID, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return diagnose(ctx, tx, c)
	}
	res, err = tx.ExecContext(ctx, `UPDATE scheduler_job_attempts
		SET dispatch_id = ?, linked_at = ?
		WHERE claim_token = ? AND dispatch_id IS NULL AND linked_at IS NULL`,
		dispatchID, now.Unix(), c.Token)
	if err != nil {
		return fmt.Errorf("link %s: attempt: %w", c.JobID, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrLostClaim
	}
	return nil
}

// Link is LinkTx in its own immediate transaction.
func (s *Store) Link(ctx context.Context, c *Claim, dispatchID string, now time.Time) error {
	return s.inTx(ctx, func(tx *db.ImmediateTx) error {
		return LinkTx(ctx, tx, c, dispatchID, now)
	})
}

// MarkRunning moves a linked launching job to running.
func (s *Store) MarkRunning(ctx context.Context, c *Claim, now time.Time) error {
	return s.inTx(ctx, func(tx *db.ImmediateTx) error {
		res, err := tx.ExecContext(ctx, `UPDATE scheduler_jobs
			SET status = 'running', started_at = ?, updated_at = ?
			WHERE id = ? AND claim_token = ? AND status = 'launching' AND dispatch_id IS NOT NULL AND `+epochGuard,
			now.Unix(), now.Unix(), c.JobID, c.Token, c.Epoch)
		if err != nil {
			return fmt.Errorf("mark running %s: %w", c.JobID, err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return diagnose(ctx, tx, c)
		}
		return nil
	})
}

// attemptWrite runs one guarded update of the claim's open attempt and
// reports whether it changed a row. set must end with the column assignments
// and extra with any further predicates; args cover set then extra.
func (s *Store) attemptWrite(ctx context.Context, c *Claim, set, extra string, args ...any) (bool, error) {
	q := `UPDATE scheduler_job_attempts SET ` + set + `
		WHERE claim_token = ? AND ended_at IS NULL` + extra + `
		  AND EXISTS (SELECT 1 FROM scheduler_jobs j
		              WHERE j.id = scheduler_job_attempts.job_id AND j.claim_token = scheduler_job_attempts.claim_token
		                AND j.status IN ` + inFlightSQL + `)
		  AND ` + epochGuard
	// args: set values, then token, then extra values, then epoch.
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return false, fmt.Errorf("attempt write %s/%d: %w", c.JobID, c.AttemptNo, err)
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

var validLaunchResults = map[string]bool{
	LaunchRejected: true, LaunchHookAborted: true, LaunchSpawnError: true, LaunchStarted: true,
}

// RecordLaunchResult persists the spawn outcome before anything else depends
// on it. It writes once; false means a result was already recorded or the
// claim is gone.
func (s *Store) RecordLaunchResult(ctx context.Context, c *Claim, result string) (bool, error) {
	if !validLaunchResults[result] {
		return false, fmt.Errorf("record launch result: invalid value %q", result)
	}
	return s.attemptWrite(ctx, c, `launch_result = ?`, ` AND launch_result IS NULL`,
		result, c.Token, c.Epoch)
}

// MarkExited records, once, that the worker process was seen to exit.
func (s *Store) MarkExited(ctx context.Context, c *Claim, now time.Time) (bool, error) {
	return s.attemptWrite(ctx, c, `exited_at = ?`, ` AND exited_at IS NULL`,
		now.Unix(), c.Token, c.Epoch)
}

// WriteEvidence records the evidence classification once (WHERE evidence_class
// IS NULL); a second write changes nothing.
func (s *Store) WriteEvidence(ctx context.Context, c *Claim, class, sha, source string) (bool, error) {
	return s.attemptWrite(ctx, c, `evidence_class = ?, evidence_sha256 = ?, evidence_source = ?`, ` AND evidence_class IS NULL`,
		class, sha, source, c.Token, c.Epoch)
}

// jobState is the job row as read inside a transaction.
type jobState struct {
	status      JobStatus
	token       string
	seq         int
	crashes     int
	policy      string
	idempotent  bool
	blocked     string
	lease       *int64
	runnerEpoch int64
	legacy      bool
}

func loadState(ctx context.Context, tx execer, id string) (*jobState, error) {
	var (
		st        jobState
		status    string
		idem      int
		lease     sql.NullInt64
		legacyInt int
	)
	err := tx.QueryRowContext(ctx, `SELECT j.status, COALESCE(j.claim_token, ''), j.attempt_seq, j.crash_requeues,
			COALESCE(j.retry_policy, ''), j.idempotent, COALESCE(j.retry_blocked, ''), j.lease_epoch,
			COALESCE((SELECT epoch FROM scheduler_runner WHERE id = 1), 0),
			COALESCE((SELECT a.legacy FROM scheduler_job_attempts a WHERE a.claim_token = j.claim_token), 0)
		FROM scheduler_jobs j WHERE j.id = ?`, id).
		Scan(&status, &st.token, &st.seq, &st.crashes, &st.policy, &idem, &st.blocked, &lease, &st.runnerEpoch, &legacyInt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load job %s: %w", id, err)
	}
	st.status = JobStatus(status)
	st.idempotent = idem != 0
	st.legacy = legacyInt != 0
	if lease.Valid {
		v := lease.Int64
		st.lease = &v
	}
	return &st, nil
}

func isInFlight(s JobStatus) bool {
	return s == StatusLaunching || s == StatusRunning || s == StatusCancelling
}

// endAttempt closes the open attempt of the claim token.
func endAttempt(ctx context.Context, tx execer, token, class, errMsg string, counts bool, now time.Time) error {
	res, err := tx.ExecContext(ctx, `UPDATE scheduler_job_attempts
		SET ended_at = ?, outcome_class = ?, error_msg = ?, counts_toward_limit = ?
		WHERE claim_token = ? AND ended_at IS NULL`,
		now.Unix(), class, nullString(errMsg), boolInt(counts), token)
	if err != nil {
		return fmt.Errorf("end attempt: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrLostClaim
	}
	return nil
}

// finish ends the open attempt with class and either returns the job to
// pending or makes it terminal. The job update is guarded by the token, the
// status read in this transaction and (when guardEpoch is set) the epoch.
func finish(ctx context.Context, tx execer, id string, st *jobState, class, errMsg, blocked string,
	now time.Time, rnd func() float64, guardEpoch int64) (*FinalizeResult, error) {

	if blocked == "" {
		blocked = st.blocked
	}
	if st.status == StatusCancelling {
		class = ClassCancelled
	}
	var (
		retry bool
		pol   RetryPolicy
	)
	if st.status != StatusCancelling {
		var perr error
		pol, perr = ParseRetryPolicy(st.policy, st.idempotent)
		if perr != nil {
			// A stored policy was validated at submit; a damaged one must not
			// retry, so it behaves as the default.
			pol = DefaultRetryPolicy()
		}
		retry = ShouldRetry(pol, class, st.seq, st.crashes, blocked != "")
	}
	if err := endAttempt(ctx, tx, st.token, class, errMsg, true, now); err != nil {
		return nil, err
	}
	var (
		res *sql.Result
		out FinalizeResult
	)
	_ = res
	if retry {
		delay := BackoffDelay(pol, class, st.seq, rnd)
		nb := now.Unix() + int64(math.Ceil(delay.Seconds()))
		out = FinalizeResult{Status: StatusPending, Retry: true, NotBefore: time.Unix(nb, 0)}
		r, err := tx.ExecContext(ctx, `UPDATE scheduler_jobs
			SET status = 'pending', not_before = ?, error_msg = ?, claim_token = NULL, lease_epoch = NULL,
			    dispatch_id = NULL, updated_at = ?
			WHERE id = ? AND claim_token = ? AND status = ? AND `+epochGuard,
			nb, nullString(errMsg), now.Unix(), id, st.token, string(st.status), guardEpoch)
		if err != nil {
			return nil, fmt.Errorf("finalize %s: %w", id, err)
		}
		if n, _ := r.RowsAffected(); n != 1 {
			return nil, ErrLostClaim
		}
		return &out, nil
	}
	final := terminalStatus(class)
	out = FinalizeResult{Status: final}
	msg := errMsg
	if class == ClassOK {
		msg = ""
	}
	r, err := tx.ExecContext(ctx, `UPDATE scheduler_jobs
		SET status = ?, outcome_class = ?, error_msg = ?, completed_at = ?, retry_blocked = ?,
		    claim_token = NULL, lease_epoch = NULL, dispatch_id = NULL, updated_at = ?
		WHERE id = ? AND claim_token = ? AND status = ? AND `+epochGuard,
		string(final), class, nullString(msg), now.Unix(), nullString(blocked), now.Unix(),
		id, st.token, string(st.status), guardEpoch)
	if err != nil {
		return nil, fmt.Errorf("finalize %s: %w", id, err)
	}
	if n, _ := r.RowsAffected(); n != 1 {
		return nil, ErrLostClaim
	}
	return &out, nil
}

// Finalize ends the claim's attempt. A cancelling job always ends cancelled
// and never retries; otherwise the class decides between pending (with a
// backoff) and a terminal status. Claim state is cleared either way.
func (s *Store) Finalize(ctx context.Context, c *Claim, p FinalizeParams) (*FinalizeResult, error) {
	if !finishClasses[p.Class] {
		return nil, fmt.Errorf("finalize %s: unknown outcome class %q", c.JobID, p.Class)
	}
	var out *FinalizeResult
	err := s.inTx(ctx, func(tx *db.ImmediateTx) error {
		st, err := loadState(ctx, tx, c.JobID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return ErrLostClaim
			}
			return err
		}
		if st.token == "" || st.token != c.Token || st.runnerEpoch != c.Epoch || !isInFlight(st.status) {
			return ErrLostClaim
		}
		out, err = finish(ctx, tx, c.JobID, st, p.Class, p.Error, p.RetryBlocked, p.Now, p.Rand, c.Epoch)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

var quarantineReasons = map[string]bool{
	ReasonRunnerInconsistent: true, ReasonSurvivorUnknown: true,
	ReasonLegacyUnknown: true, ReasonLegacyRunning: true,
}

// Recover applies one recovery decision to an in-flight row left by an older
// runner epoch. It requires p.Epoch to be the current epoch and the row's
// lease to be older. Only the runner-died-before-link case is an excused
// crash: it doesn't count toward the limit, it raises crash_requeues, and the
// fourth such crash abandons the job.
func (s *Store) Recover(ctx context.Context, p RecoverParams) (*FinalizeResult, error) {
	switch p.Action {
	case ActCancelled, ActPrestartAbort, ActFinalize:
	case ActQuarantine:
		if !quarantineReasons[p.Reason] {
			return nil, fmt.Errorf("%w: unknown quarantine reason %q", ErrBadRecovery, p.Reason)
		}
	default:
		return nil, fmt.Errorf("%w: %s is applied by the caller, not by Recover", ErrBadRecovery, p.Action)
	}
	if p.Action == ActFinalize && !finishClasses[p.Class] {
		return nil, fmt.Errorf("%w: unknown outcome class %q", ErrBadRecovery, p.Class)
	}
	var out *FinalizeResult
	err := s.inTx(ctx, func(tx *db.ImmediateTx) error {
		st, err := loadState(ctx, tx, p.JobID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return ErrLostClaim
			}
			return err
		}
		if st.runnerEpoch != p.Epoch || !InRecoveryScope(st.status, st.lease, p.Epoch) || st.token == "" {
			return ErrLostClaim
		}
		now := p.Now
		switch p.Action {
		case ActCancelled:
			if st.status != StatusCancelling {
				return fmt.Errorf("%w: cancelled recovery needs a cancelling job, not %s", ErrBadRecovery, st.status)
			}
			out, err = finish(ctx, tx, p.JobID, st, ClassCancelled, "", p.RetryBlocked, now, p.Rand, p.Epoch)
		case ActPrestartAbort:
			out, err = recoverPrestartAbort(ctx, tx, p, st)
		case ActFinalize:
			out, err = recoverFinalize(ctx, tx, p, st)
		case ActQuarantine:
			out, err = quarantine(ctx, tx, p, st, p.Reason)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func recoverPrestartAbort(ctx context.Context, tx execer, p RecoverParams, st *jobState) (*FinalizeResult, error) {
	if st.status == StatusCancelling {
		// A cancel arrived before the runner died: honour it.
		return finish(ctx, tx, p.JobID, st, ClassCancelled, "", p.RetryBlocked, p.Now, p.Rand, p.Epoch)
	}
	if err := endAttempt(ctx, tx, st.token, ClassPrestartAbort, "runner exited before the dispatch was linked", false, p.Now); err != nil {
		return nil, err
	}
	if st.crashes >= MaxCrashRequeues {
		r, err := tx.ExecContext(ctx, `UPDATE scheduler_jobs
			SET status = 'failed', outcome_class = ?, error_msg = ?, completed_at = ?,
			    claim_token = NULL, lease_epoch = NULL, dispatch_id = NULL, updated_at = ?
			WHERE id = ? AND claim_token = ? AND status = ? AND `+epochGuard,
			ClassAbandoned, "runner crashed before launch too many times", p.Now.Unix(), p.Now.Unix(),
			p.JobID, st.token, string(st.status), p.Epoch)
		if err != nil {
			return nil, fmt.Errorf("recover %s: %w", p.JobID, err)
		}
		if n, _ := r.RowsAffected(); n != 1 {
			return nil, ErrLostClaim
		}
		return &FinalizeResult{Status: StatusFailed}, nil
	}
	r, err := tx.ExecContext(ctx, `UPDATE scheduler_jobs
		SET status = 'pending', crash_requeues = crash_requeues + 1,
		    claim_token = NULL, lease_epoch = NULL, dispatch_id = NULL, updated_at = ?
		WHERE id = ? AND claim_token = ? AND status = ? AND `+epochGuard,
		p.Now.Unix(), p.JobID, st.token, string(st.status), p.Epoch)
	if err != nil {
		return nil, fmt.Errorf("recover %s: %w", p.JobID, err)
	}
	if n, _ := r.RowsAffected(); n != 1 {
		return nil, ErrLostClaim
	}
	return &FinalizeResult{Status: StatusPending, Retry: true}, nil
}

func recoverFinalize(ctx context.Context, tx execer, p RecoverParams, st *jobState) (*FinalizeResult, error) {
	if !st.legacy {
		return finish(ctx, tx, p.JobID, st, p.Class, "", p.RetryBlocked, p.Now, p.Rand, p.Epoch)
	}
	// The legacy rule: a definite class is final with no retry; an
	// indeterminate or unknown one needs an operator.
	switch p.Class {
	case ClassIndeterminate, ClassUnknown:
		return quarantine(ctx, tx, p, st, ReasonLegacyUnknown)
	}
	if st.status == StatusCancelling {
		return finish(ctx, tx, p.JobID, st, ClassCancelled, "", p.RetryBlocked, p.Now, p.Rand, p.Epoch)
	}
	if err := endAttempt(ctx, tx, st.token, p.Class, "", true, p.Now); err != nil {
		return nil, err
	}
	final := terminalStatus(p.Class)
	r, err := tx.ExecContext(ctx, `UPDATE scheduler_jobs
		SET status = ?, outcome_class = ?, completed_at = ?, claim_token = NULL, lease_epoch = NULL,
		    dispatch_id = NULL, updated_at = ?
		WHERE id = ? AND claim_token = ? AND status = ? AND `+epochGuard,
		string(final), p.Class, p.Now.Unix(), p.Now.Unix(), p.JobID, st.token, string(st.status), p.Epoch)
	if err != nil {
		return nil, fmt.Errorf("recover %s: %w", p.JobID, err)
	}
	if n, _ := r.RowsAffected(); n != 1 {
		return nil, ErrLostClaim
	}
	return &FinalizeResult{Status: final}, nil
}

// quarantine parks the job for an operator and keeps its claim state and open
// attempt, which are the evidence requeue and cancel work from.
func quarantine(ctx context.Context, tx execer, p RecoverParams, st *jobState, reason string) (*FinalizeResult, error) {
	blocked := p.RetryBlocked
	if blocked == "" {
		blocked = st.blocked
	}
	r, err := tx.ExecContext(ctx, `UPDATE scheduler_jobs
		SET status = 'quarantined', outcome_class = ?, retry_blocked = ?, updated_at = ?
		WHERE id = ? AND claim_token = ? AND status = ? AND `+epochGuard,
		reason, nullString(blocked), p.Now.Unix(), p.JobID, st.token, string(st.status), p.Epoch)
	if err != nil {
		return nil, fmt.Errorf("quarantine %s: %w", p.JobID, err)
	}
	if n, _ := r.RowsAffected(); n != 1 {
		return nil, ErrLostClaim
	}
	return &FinalizeResult{Status: StatusQuarantined}, nil
}

// CancelJob cancels a job from whatever state it is in, as one compare-and-swap
// transaction, so a cancel that raced a claim can't overwrite the claim.
// Pending and quarantined jobs become cancelled directly; launching and running
// jobs become cancelling and a runner (or the operator's --force) finishes the
// cancel; cancelling is a no-op; a terminal job is ErrAlreadyTerminal.
func (s *Store) CancelJob(ctx context.Context, id string, now time.Time) (*CancelResult, error) {
	var out *CancelResult
	err := s.inTx(ctx, func(tx *db.ImmediateTx) error {
		st, err := loadState(ctx, tx, id)
		if err != nil {
			return err
		}
		switch st.status {
		case StatusPending:
			r, err := tx.ExecContext(ctx, `UPDATE scheduler_jobs
				SET status = 'cancelled', outcome_class = 'cancelled', completed_at = ?, updated_at = ?
				WHERE id = ? AND status = 'pending'`, now.Unix(), now.Unix(), id)
			if err != nil {
				return fmt.Errorf("cancel %s: %w", id, err)
			}
			if n, _ := r.RowsAffected(); n != 1 {
				return ErrLostClaim
			}
			out = &CancelResult{Status: StatusCancelled}
		case StatusLaunching, StatusRunning:
			r, err := tx.ExecContext(ctx, `UPDATE scheduler_jobs
				SET status = 'cancelling', updated_at = ?
				WHERE id = ? AND status = ?`, now.Unix(), id, string(st.status))
			if err != nil {
				return fmt.Errorf("cancel %s: %w", id, err)
			}
			if n, _ := r.RowsAffected(); n != 1 {
				return ErrLostClaim
			}
			out = &CancelResult{Status: StatusCancelling, InFlight: true}
		case StatusCancelling:
			out = &CancelResult{Status: StatusCancelling, InFlight: true}
		case StatusQuarantined:
			if st.token != "" {
				if _, err := tx.ExecContext(ctx, `UPDATE scheduler_job_attempts
					SET ended_at = ?, outcome_class = ?
					WHERE claim_token = ? AND ended_at IS NULL`, now.Unix(), ClassCancelled, st.token); err != nil {
					return fmt.Errorf("cancel %s: attempt: %w", id, err)
				}
			}
			r, err := tx.ExecContext(ctx, `UPDATE scheduler_jobs
				SET status = 'cancelled', outcome_class = 'cancelled', completed_at = ?, updated_at = ?,
				    claim_token = NULL, lease_epoch = NULL, dispatch_id = NULL
				WHERE id = ? AND status = 'quarantined'`, now.Unix(), now.Unix(), id)
			if err != nil {
				return fmt.Errorf("cancel %s: %w", id, err)
			}
			if n, _ := r.RowsAffected(); n != 1 {
				return ErrLostClaim
			}
			out = &CancelResult{Status: StatusCancelled}
		default:
			return ErrAlreadyTerminal
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Requeue returns a quarantined job to pending after an operator confirmed
// that no worker is running. The open attempt is closed as released, claim
// state is cleared, and attempt_seq is untouched, so the next claim is a new
// attempt number.
func (s *Store) Requeue(ctx context.Context, id string, confirmNotRunning bool, now time.Time) error {
	if !confirmNotRunning {
		return ErrConfirmRequired
	}
	return s.inTx(ctx, func(tx *db.ImmediateTx) error {
		st, err := loadState(ctx, tx, id)
		if err != nil {
			return err
		}
		if st.status != StatusQuarantined {
			return ErrNotQuarantined
		}
		if st.token != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE scheduler_job_attempts
				SET ended_at = ?, outcome_class = ?
				WHERE claim_token = ? AND ended_at IS NULL`, now.Unix(), ClassReleased, st.token); err != nil {
				return fmt.Errorf("requeue %s: attempt: %w", id, err)
			}
		}
		r, err := tx.ExecContext(ctx, `UPDATE scheduler_jobs
			SET status = 'pending', outcome_class = NULL, not_before = 0, updated_at = ?,
			    claim_token = NULL, lease_epoch = NULL, dispatch_id = NULL
			WHERE id = ? AND status = 'quarantined'`, now.Unix(), id)
		if err != nil {
			return fmt.Errorf("requeue %s: %w", id, err)
		}
		if n, _ := r.RowsAffected(); n != 1 {
			return ErrNotQuarantined
		}
		return nil
	})
}

// Unblock clears retry_blocked after an operator confirmed that no worker of
// the job is running, so a claim may take the job again.
func (s *Store) Unblock(ctx context.Context, id string, confirmNotRunning bool) error {
	if !confirmNotRunning {
		return ErrConfirmRequired
	}
	return s.inTx(ctx, func(tx *db.ImmediateTx) error {
		r, err := tx.ExecContext(ctx, `UPDATE scheduler_jobs
			SET retry_blocked = NULL, updated_at = ?
			WHERE id = ? AND retry_blocked IS NOT NULL`, time.Now().Unix(), id)
		if err != nil {
			return fmt.Errorf("unblock %s: %w", id, err)
		}
		if n, _ := r.RowsAffected(); n == 1 {
			return nil
		}
		var one int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM scheduler_jobs WHERE id = ?`, id).Scan(&one); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return ErrNotBlocked
	})
}
