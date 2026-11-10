package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// Store provides SQLite persistence for scheduler jobs.
type Store struct {
	db *sql.DB
}

// NewStore creates a store using the given *sql.DB.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// jobColumns is the column list every read uses; scanJob matches its order.
const jobColumns = `id, status, priority, agent_type, session_name, batch_id, dispatch_id, spawn_opts,
	max_retries, retry_count, error_msg, created_at, started_at, completed_at,
	idempotency_key, not_before, attempt_seq, crash_requeues, claim_token, lease_epoch,
	retry_policy, idempotent, outcome_class, output_base, project_dir, retry_blocked, updated_at`

// Create inserts a new scheduler job record. It stamps updated_at, which also
// marks the row as already normalized for migration 041's data chunks.
func (s *Store) Create(ctx context.Context, job *SpawnJob) error {
	updated := job.UpdatedAt
	if updated.IsZero() {
		updated = time.Now()
	}
	var notBefore int64
	if !job.NotBefore.IsZero() {
		notBefore = job.NotBefore.Unix()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO scheduler_jobs (id, status, priority, agent_type, session_name, batch_id, dispatch_id, spawn_opts,
			max_retries, retry_count, error_msg, created_at, started_at, completed_at,
			idempotency_key, not_before, retry_policy, idempotent, output_base, project_dir, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		job.ID,
		string(job.Status),
		int(job.Priority),
		job.AgentType,
		nullString(job.SessionName),
		nullString(job.BatchID),
		nullString(job.DispatchID),
		job.SpawnOpts,
		job.MaxRetries,
		job.RetryCount,
		nullString(job.Error),
		job.CreatedAt.Unix(),
		nullUnix(job.StartedAt),
		nullUnix(job.CompletedAt),
		nullString(job.IdempotencyKey),
		notBefore,
		nullString(job.RetryPolicy),
		boolInt(job.Idempotent),
		nullString(job.OutputBase),
		nullString(job.ProjectDir),
		updated.Unix(),
	)
	if err != nil {
		return fmt.Errorf("store.create: %w", err)
	}
	return nil
}

// Get retrieves a job by ID. A missing job returns sql.ErrNoRows.
func (s *Store) Get(ctx context.Context, id string) (*SpawnJob, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM scheduler_jobs WHERE id = ?`, id)
	return scanJob(row)
}

// List returns jobs matching the given status filter (empty = all).
func (s *Store) List(ctx context.Context, status string, limit int) ([]*SpawnJob, error) {
	var query string
	var args []any

	if status != "" {
		query = `SELECT ` + jobColumns + ` FROM scheduler_jobs WHERE status = ? ORDER BY created_at DESC LIMIT ?`
		args = []any{status, limit}
	} else {
		query = `SELECT ` + jobColumns + ` FROM scheduler_jobs ORDER BY created_at DESC LIMIT ?`
		args = []any{limit}
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store.list: %w", err)
	}
	defer rows.Close()

	var jobs []*SpawnJob
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("store.list: scan: %w", err)
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.list: rows: %w", err)
	}
	return jobs, nil
}

// CountByStatus returns a map of status -> count for all jobs.
func (s *Store) CountByStatus(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT status, COUNT(*) FROM scheduler_jobs GROUP BY status`)
	if err != nil {
		return nil, fmt.Errorf("store.count-by-status: %w", err)
	}
	defer rows.Close()

	counts := make(map[string]int)
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return nil, fmt.Errorf("store.count-by-status: scan: %w", err)
		}
		counts[status] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.count-by-status: rows: %w", err)
	}
	return counts, nil
}

// Prune deletes completed/failed/cancelled jobs older than the given age.
func (s *Store) Prune(ctx context.Context, olderThan time.Duration) (int64, error) {
	cutoff := time.Now().Add(-olderThan).Unix()
	result, err := s.db.ExecContext(ctx,
		`DELETE FROM scheduler_jobs WHERE status IN ('completed', 'failed', 'cancelled') AND completed_at < ?`,
		cutoff)
	if err != nil {
		return 0, fmt.Errorf("store.prune: %w", err)
	}
	return result.RowsAffected()
}

// rowScanner is satisfied by *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanJob scans one row selected with jobColumns into a SpawnJob.
func scanJob(row rowScanner) (*SpawnJob, error) {
	var (
		j              SpawnJob
		status         string
		priority       int
		sessionName    sql.NullString
		batchID        sql.NullString
		dispatchID     sql.NullString
		errorMsg       sql.NullString
		createdAt      int64
		startedAt      sql.NullInt64
		completedAt    sql.NullInt64
		idempotencyKey sql.NullString
		notBefore      int64
		claimToken     sql.NullString
		leaseEpoch     sql.NullInt64
		retryPolicy    sql.NullString
		idempotent     int
		outcomeClass   sql.NullString
		outputBase     sql.NullString
		projectDir     sql.NullString
		retryBlocked   sql.NullString
		updatedAt      sql.NullInt64
	)

	err := row.Scan(
		&j.ID, &status, &priority, &j.AgentType,
		&sessionName, &batchID, &dispatchID, &j.SpawnOpts,
		&j.MaxRetries, &j.RetryCount, &errorMsg,
		&createdAt, &startedAt, &completedAt,
		&idempotencyKey, &notBefore, &j.AttemptSeq, &j.CrashRequeues, &claimToken, &leaseEpoch,
		&retryPolicy, &idempotent, &outcomeClass, &outputBase, &projectDir, &retryBlocked, &updatedAt,
	)
	if err != nil {
		return nil, err
	}

	// The persisted queue currently contains dispatch jobs only. Restore the
	// runtime context required by Scheduler after loading an admitted job.
	j.Type = JobTypeDispatch
	j.ctx, j.cancel = context.WithCancel(context.Background())
	j.Metadata = make(map[string]interface{})
	j.Status = JobStatus(status)
	j.Priority = JobPriority(priority)
	j.SessionName = sessionName.String
	j.BatchID = batchID.String
	j.DispatchID = dispatchID.String
	j.Error = errorMsg.String
	j.CreatedAt = time.Unix(createdAt, 0)
	if startedAt.Valid {
		j.StartedAt = time.Unix(startedAt.Int64, 0)
	}
	if completedAt.Valid {
		j.CompletedAt = time.Unix(completedAt.Int64, 0)
	}
	j.IdempotencyKey = idempotencyKey.String
	if notBefore > 0 {
		j.NotBefore = time.Unix(notBefore, 0)
	}
	j.ClaimToken = claimToken.String
	if leaseEpoch.Valid {
		e := leaseEpoch.Int64
		j.LeaseEpoch = &e
	}
	j.RetryPolicy = retryPolicy.String
	j.Idempotent = idempotent != 0
	j.OutcomeClass = outcomeClass.String
	j.OutputBase = outputBase.String
	j.ProjectDir = projectDir.String
	j.RetryBlocked = retryBlocked.String
	if updatedAt.Valid {
		j.UpdatedAt = time.Unix(updatedAt.Int64, 0)
	}

	return &j, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

func nullUnix(t time.Time) sql.NullInt64 {
	if t.IsZero() {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.Unix(), Valid: true}
}

// MarshalSpawnOpts serializes spawn options to JSON for persistence.
func MarshalSpawnOpts(opts any) (string, error) {
	b, err := json.Marshal(opts)
	if err != nil {
		return "", fmt.Errorf("marshal spawn opts: %w", err)
	}
	return string(b), nil
}

// UnmarshalSpawnOpts deserializes spawn options from JSON.
func UnmarshalSpawnOpts(data string, out any) error {
	if err := json.Unmarshal([]byte(data), out); err != nil {
		return fmt.Errorf("unmarshal spawn opts: %w", err)
	}
	return nil
}
