package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/mistakeknot/intercore/internal/db"
)

// Runner-side store operations (plan sections 5.3 and 5.4): the singleton row,
// its epoch, the heartbeat, the rebind quarantine, and the reads the runner
// loop needs. Every write to a scheduler table made on behalf of a running
// runner carries the epoch guard; the acquire and rebind transactions are the
// two that change the epoch itself.

var (
	// ErrIdentityMismatch: the singleton's recorded db_path, db_identity,
	// net_ns or uid differ from this process's. Only --rebind-db changes them.
	ErrIdentityMismatch = errors.New("scheduler: runner row belongs to a different database identity")
	// ErrSuperseded: the runner's epoch is no longer current.
	ErrSuperseded = errors.New("scheduler: runner superseded")
)

// RunnerIdentity is what a runner records about itself and the database it
// has locked. DBIdentity is "st_dev:st_ino"; NetNS is the inode of
// /proc/self/ns/net.
type RunnerIdentity struct {
	RunnerID     string
	PID          int
	PIDStartTime uint64
	BootID       string
	Host         string
	Version      string
	DBPath       string
	DBIdentity   string
	NetNS        uint64
	UID          int
}

// RunnerRow is the singleton scheduler_runner row.
type RunnerRow struct {
	Epoch        int64
	RunnerID     string
	PID          int
	PIDStartTime uint64
	BootID       string
	Host         string
	StartedAt    int64
	HeartbeatAt  int64
	Version      string
	State        string
	DBIdentity   string
	DBPath       string
	QueueID      string
	UID          int64
	NetNS        uint64
}

// ReadRunner returns the singleton row.
func (s *Store) ReadRunner(ctx context.Context) (*RunnerRow, error) {
	return readRunner(ctx, s.db)
}

type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func readRunner(ctx context.Context, q queryRower) (*RunnerRow, error) {
	var (
		r                                    RunnerRow
		runnerID, bootID, host, ver, ident   sql.NullString
		dbPath                               sql.NullString
		pid, started, hb, uid, netns, starts sql.NullInt64
	)
	err := q.QueryRowContext(ctx, `SELECT epoch, runner_id, pid, pid_starttime, boot_id, host, started_at,
			heartbeat_at, version, state, db_identity, db_path, queue_id, uid, net_ns
		FROM scheduler_runner WHERE id = 1`).
		Scan(&r.Epoch, &runnerID, &pid, &starts, &bootID, &host, &started, &hb, &ver, &r.State, &ident, &dbPath, &r.QueueID, &uid, &netns)
	if err != nil {
		return nil, fmt.Errorf("read runner row: %w", err)
	}
	r.RunnerID, r.BootID, r.Host, r.Version = runnerID.String, bootID.String, host.String, ver.String
	r.DBIdentity, r.DBPath = ident.String, dbPath.String
	r.PID, r.PIDStartTime = int(pid.Int64), uint64(starts.Int64)
	r.StartedAt, r.HeartbeatAt, r.UID, r.NetNS = started.Int64, hb.Int64, uid.Int64, uint64(netns.Int64)
	return &r, nil
}

// identityMatches reports whether a recorded row either has no identity yet
// (first acquire after the migration) or equals id on every field.
func (r *RunnerRow) identityMatches(id RunnerIdentity) bool {
	if r.DBPath == "" && r.DBIdentity == "" {
		return true
	}
	return r.DBPath == id.DBPath && r.DBIdentity == id.DBIdentity &&
		r.NetNS == id.NetNS && r.UID == int64(id.UID)
}

// AcquireEpoch bumps the epoch in one transaction and records the runner. It
// is called only after both lifetime locks are held. It refuses a row whose
// identity belongs to another database path, inode, network namespace or uid,
// which is what a copied or moved database looks like (ErrIdentityMismatch).
// The row as it was before the bump is returned for diagnostics.
func (s *Store) AcquireEpoch(ctx context.Context, id RunnerIdentity, now time.Time) (epoch int64, prior *RunnerRow, err error) {
	err = s.inTx(ctx, func(tx *db.ImmediateTx) error {
		p, err := readRunner(ctx, tx)
		if err != nil {
			return err
		}
		if !p.identityMatches(id) {
			return fmt.Errorf("%w: recorded %s (%s) uid %d netns %d, this process %s (%s) uid %d netns %d; use --rebind-db",
				ErrIdentityMismatch, p.DBPath, p.DBIdentity, p.UID, p.NetNS, id.DBPath, id.DBIdentity, id.UID, id.NetNS)
		}
		if err := writeRunner(ctx, tx, p.Epoch+1, id, now, "running"); err != nil {
			return err
		}
		prior = p
		epoch = p.Epoch + 1
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return epoch, prior, nil
}

func writeRunner(ctx context.Context, tx execer, epoch int64, id RunnerIdentity, now time.Time, state string) error {
	r, err := tx.ExecContext(ctx, `UPDATE scheduler_runner
		SET epoch = ?, runner_id = ?, pid = ?, pid_starttime = ?, boot_id = ?, host = ?, started_at = ?,
		    heartbeat_at = ?, version = ?, state = ?, db_identity = ?, db_path = ?, uid = ?, net_ns = ?
		WHERE id = 1`,
		epoch, id.RunnerID, id.PID, int64(id.PIDStartTime), id.BootID, id.Host, now.Unix(),
		now.Unix(), id.Version, state, id.DBIdentity, id.DBPath, id.UID, int64(id.NetNS))
	if err != nil {
		return fmt.Errorf("write runner row: %w", err)
	}
	if n, _ := r.RowsAffected(); n != 1 {
		return fmt.Errorf("write runner row: no singleton row")
	}
	return nil
}

// RebindParams is --rebind-db: the one operation that changes the recorded
// identity. The caller has already verified that both locks are free and that
// the previous runner is absent.
type RebindParams struct {
	Identity RunnerIdentity
	Now      time.Time
}

// RebindResult reports what a rebind did.
type RebindResult struct {
	Epoch       int64
	Quarantined int
}

// Rebind bumps the epoch, rewrites db_path, db_identity, net_ns and uid, pauses
// the scheduler, and quarantines every in-flight row as rebind_unknown, in one
// transaction. The rows keep their claim state: that is the evidence an
// operator requeues or cancels from. The new epoch is also the runner's epoch,
// so the same process continues as the runner after the rebind commits.
func (s *Store) Rebind(ctx context.Context, p RebindParams) (*RebindResult, error) {
	var out RebindResult
	err := s.inTx(ctx, func(tx *db.ImmediateTx) error {
		cur, err := readRunner(ctx, tx)
		if err != nil {
			return err
		}
		if err := writeRunner(ctx, tx, cur.Epoch+1, p.Identity, p.Now, "running"); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO state (key, scope_id, payload, updated_at)
			VALUES ('scheduler_paused', 'global', 'true', ?)
			ON CONFLICT(key, scope_id) DO UPDATE SET payload = 'true', updated_at = excluded.updated_at`, p.Now.Unix()); err != nil {
			return fmt.Errorf("rebind: pause: %w", err)
		}
		r, err := tx.ExecContext(ctx, `UPDATE scheduler_jobs
			SET status = 'quarantined', outcome_class = ?, updated_at = ?
			WHERE status IN `+inFlightSQL, ReasonRebindUnknown, p.Now.Unix())
		if err != nil {
			return fmt.Errorf("rebind: quarantine: %w", err)
		}
		n, _ := r.RowsAffected()
		out = RebindResult{Epoch: cur.Epoch + 1, Quarantined: int(n)}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Heartbeat refreshes heartbeat_at for the runner at epoch. False means the
// row now carries another epoch: the runner has been superseded and must exit.
func (s *Store) Heartbeat(ctx context.Context, epoch int64, now time.Time) (bool, error) {
	r, err := s.db.ExecContext(ctx, `UPDATE scheduler_runner SET heartbeat_at = ? WHERE id = 1 AND epoch = ?`, now.Unix(), epoch)
	if err != nil {
		return false, fmt.Errorf("heartbeat: %w", err)
	}
	n, _ := r.RowsAffected()
	return n == 1, nil
}

// SetRunnerState moves the runner row to draining or stopped, guarded by epoch.
func (s *Store) SetRunnerState(ctx context.Context, epoch int64, state string) (bool, error) {
	if state != "running" && state != "draining" && state != "stopped" {
		return false, fmt.Errorf("set runner state: invalid state %q", state)
	}
	r, err := s.db.ExecContext(ctx, `UPDATE scheduler_runner SET state = ? WHERE id = 1 AND epoch = ?`, state, epoch)
	if err != nil {
		return false, fmt.Errorf("set runner state: %w", err)
	}
	n, _ := r.RowsAffected()
	return n == 1, nil
}

// CurrentEpoch reads the singleton's epoch.
func (s *Store) CurrentEpoch(ctx context.Context) (int64, error) {
	var e int64
	if err := s.db.QueryRowContext(ctx, `SELECT epoch FROM scheduler_runner WHERE id = 1`).Scan(&e); err != nil {
		return 0, fmt.Errorf("read epoch: %w", err)
	}
	return e, nil
}

// IsPaused reports the global pause flag.
func (s *Store) IsPaused(ctx context.Context) (bool, error) {
	var p string
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE((SELECT payload FROM state
		WHERE key = 'scheduler_paused' AND scope_id = 'global'), 'false')`).Scan(&p)
	if err != nil {
		return false, fmt.Errorf("read pause flag: %w", err)
	}
	return p == "true", nil
}

// ClaimCandidates lists up to limit job IDs a claim could take now, in claim
// order: priority, then creation time, then ID. It is a read; Claim decides.
func (s *Store) ClaimCandidates(ctx context.Context, now time.Time, limit int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM scheduler_jobs
		WHERE status = 'pending' AND retry_blocked IS NULL AND not_before <= ?
		ORDER BY priority ASC, created_at ASC, id ASC LIMIT ?`, now.Unix(), limit)
	if err != nil {
		return nil, fmt.Errorf("claim candidates: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// FailConfig fails a still-pending, unclaimed job as config_error in one
// guarded transaction. No attempt row and no dispatch row exist for it: this
// is the durable record that the job never started. It honors the same pause,
// due-time and retry-block predicates as Claim, so a paused queue is untouched.
func (s *Store) FailConfig(ctx context.Context, jobID string, epoch int64, msg string, now time.Time) (bool, error) {
	r, err := s.db.ExecContext(ctx, `UPDATE scheduler_jobs
		SET status = 'failed', outcome_class = 'config_error', error_msg = ?, completed_at = ?, updated_at = ?
		WHERE id = ? AND status = 'pending' AND claim_token IS NULL AND retry_blocked IS NULL AND not_before <= ?
		  AND `+epochGuard+`
		  AND COALESCE((SELECT payload FROM state WHERE key = 'scheduler_paused' AND scope_id = 'global'), 'false') <> 'true'`,
		msg, now.Unix(), now.Unix(), jobID, now.Unix(), epoch)
	if err != nil {
		return false, fmt.Errorf("fail config %s: %w", jobID, err)
	}
	n, _ := r.RowsAffected()
	return n == 1, nil
}

// InFlightCount counts jobs a claim token is live for at epoch.
func (s *Store) InFlightCount(ctx context.Context, epoch int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM scheduler_jobs
		WHERE status IN `+inFlightSQL+` AND lease_epoch = ?`, epoch).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("in-flight count: %w", err)
	}
	return n, nil
}

// CancellingClaim is a cancelling row owned by the epoch.
type CancellingClaim struct {
	JobID string
	Token string
}

// CancellingClaims lists the cancelling rows whose lease is the given epoch.
func (s *Store) CancellingClaims(ctx context.Context, epoch int64) ([]CancellingClaim, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, claim_token FROM scheduler_jobs
		WHERE status = 'cancelling' AND lease_epoch = ? AND claim_token IS NOT NULL`, epoch)
	if err != nil {
		return nil, fmt.Errorf("cancelling claims: %w", err)
	}
	defer rows.Close()
	var out []CancellingClaim
	for rows.Next() {
		var c CancellingClaim
		if err := rows.Scan(&c.JobID, &c.Token); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// AttemptRow is the persisted state of one attempt, read by token.
type AttemptRow struct {
	JobID         string
	AttemptNo     int
	Token         string
	LeaseEpoch    int64
	DispatchID    string
	LinkedAt      int64
	OutputFile    string
	CgroupPath    string
	Containment   string
	LaunchResult  string
	ExitedAt      int64
	EndedAt       int64
	EvidenceClass string
	EvidenceSHA   string
	EvidenceSrc   string
	JobStatus     JobStatus
	JobEpoch      int64 // the job row's lease_epoch
	Legacy        bool  // a synthetic attempt made by the 040 migration for a pre-runner row
}

// AttemptByToken reads the attempt row and its job's status for a claim token.
func (s *Store) AttemptByToken(ctx context.Context, token string) (*AttemptRow, error) {
	var (
		a                                   AttemptRow
		disp, outf, cg, lr, evc, evs, evsrc sql.NullString
		linked, exited, ended, jobEpoch     sql.NullInt64
		status                              string
		legacy                              int
	)
	err := s.db.QueryRowContext(ctx, `SELECT a.job_id, a.attempt_no, a.claim_token, a.lease_epoch, a.dispatch_id,
			a.linked_at, a.output_file, a.cgroup_path, a.containment, a.launch_result, a.exited_at, a.ended_at,
			a.evidence_class, a.evidence_sha256, a.evidence_source, j.status, j.lease_epoch, a.legacy
		FROM scheduler_job_attempts a JOIN scheduler_jobs j ON j.id = a.job_id
		WHERE a.claim_token = ?`, token).
		Scan(&a.JobID, &a.AttemptNo, &a.Token, &a.LeaseEpoch, &disp, &linked, &outf, &cg, &a.Containment, &lr,
			&exited, &ended, &evc, &evs, &evsrc, &status, &jobEpoch, &legacy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("attempt by token: %w", err)
	}
	a.DispatchID, a.OutputFile, a.CgroupPath, a.LaunchResult = disp.String, outf.String, cg.String, lr.String
	a.EvidenceClass, a.EvidenceSHA, a.EvidenceSrc = evc.String, evs.String, evsrc.String
	a.LinkedAt, a.ExitedAt, a.EndedAt, a.JobEpoch = linked.Int64, exited.Int64, ended.Int64, jobEpoch.Int64
	a.JobStatus = JobStatus(status)
	a.Legacy = legacy != 0
	return &a, nil
}

// SetAttemptPaths records the attempt's output file and containment before the
// worker starts, guarded by the claim and epoch, so extinction and evidence
// reads use the stored values and never recompute them.
func (s *Store) SetAttemptPaths(ctx context.Context, c *Claim, outputFile, containment, cgroupPath string) (bool, error) {
	return s.attemptWrite(ctx, c, `output_file = ?, containment = ?, cgroup_path = ?`, ``,
		outputFile, containment, nullString(cgroupPath), c.Token, c.Epoch)
}

// InFlightClaims lists every in-flight row the epoch owns, for the repair pass.
func (s *Store) InFlightClaims(ctx context.Context, epoch int64) ([]CancellingClaim, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, claim_token FROM scheduler_jobs
		WHERE status IN `+inFlightSQL+` AND lease_epoch = ? AND claim_token IS NOT NULL`, epoch)
	if err != nil {
		return nil, fmt.Errorf("in-flight claims: %w", err)
	}
	defer rows.Close()
	var out []CancellingClaim
	for rows.Next() {
		var c CancellingClaim
		if err := rows.Scan(&c.JobID, &c.Token); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetRetryBlocked records that a survivor of the attempt may still run. It is
// guarded by token and epoch and leaves the job in flight; the finalize that
// follows keeps the marker.
func (s *Store) SetRetryBlocked(ctx context.Context, c *Claim, marker string, now time.Time) (bool, error) {
	r, err := s.db.ExecContext(ctx, `UPDATE scheduler_jobs SET retry_blocked = ?, updated_at = ?
		WHERE id = ? AND claim_token = ? AND status IN `+inFlightSQL+` AND `+epochGuard,
		marker, now.Unix(), c.JobID, c.Token, c.Epoch)
	if err != nil {
		return false, fmt.Errorf("set retry_blocked %s: %w", c.JobID, err)
	}
	n, _ := r.RowsAffected()
	return n == 1, nil
}

// ClaimFor rebuilds the Claim of an in-flight row from the database, for the
// repair pass. The epoch is the caller's own: a row of another epoch is not
// repaired by this runner.
func (s *Store) ClaimFor(ctx context.Context, token string, epoch int64) (*Claim, error) {
	a, err := s.AttemptByToken(ctx, token)
	if err != nil {
		return nil, err
	}
	if a.JobEpoch != epoch || !isInFlight(a.JobStatus) {
		return nil, ErrLostClaim
	}
	return &Claim{JobID: a.JobID, Token: token, AttemptNo: a.AttemptNo, Epoch: epoch}, nil
}

// PriorClaim is an in-flight row held by an older runner epoch.
type PriorClaim struct {
	JobID      string
	Token      string
	Status     JobStatus
	LeaseEpoch int64 // 0 for a row whose lease is NULL (migrated legacy rows)
}

// PriorEpochClaims lists the in-flight rows a runner at epoch mine recovers:
// the lease is older than mine, and a NULL lease counts as epoch 0
// (InRecoveryScope). Rows without a claim token have no attempt to recover and
// are not listed. The order is by job id, so a pass is deterministic.
func (s *Store) PriorEpochClaims(ctx context.Context, mine int64) ([]PriorClaim, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, claim_token, status, COALESCE(lease_epoch, 0) FROM scheduler_jobs
		WHERE status IN `+inFlightSQL+` AND COALESCE(lease_epoch, 0) < ? AND claim_token IS NOT NULL
		ORDER BY id`, mine)
	if err != nil {
		return nil, fmt.Errorf("prior-epoch claims: %w", err)
	}
	defer rows.Close()
	var out []PriorClaim
	for rows.Next() {
		var c PriorClaim
		var st string
		if err := rows.Scan(&c.JobID, &c.Token, &st, &c.LeaseEpoch); err != nil {
			return nil, err
		}
		c.Status = JobStatus(st)
		out = append(out, c)
	}
	return out, rows.Err()
}

// PriorClaimFor builds the Claim a runner at epoch mine uses to finalize a row
// left by an older epoch. Every guarded write checks the singleton epoch
// against Claim.Epoch, so the claim carries the recovering runner's epoch, not
// the row's. It fails with ErrLostClaim unless the row is still in recovery
// scope, which keeps a runner away from its own epoch's rows.
func (s *Store) PriorClaimFor(ctx context.Context, token string, mine int64) (*Claim, error) {
	a, err := s.AttemptByToken(ctx, token)
	if err != nil {
		return nil, err
	}
	var lease *int64
	if a.JobEpoch != 0 {
		v := a.JobEpoch
		lease = &v
	}
	if !InRecoveryScope(a.JobStatus, lease, mine) {
		return nil, ErrLostClaim
	}
	return &Claim{JobID: a.JobID, Token: token, AttemptNo: a.AttemptNo, Epoch: mine}, nil
}
