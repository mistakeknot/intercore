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

// Create inserts a new scheduler job record.
func (s *Store) Create(ctx context.Context, job *SpawnJob) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO scheduler_jobs (id, status, priority, agent_type, session_name, batch_id, dispatch_id, spawn_opts, max_retries, retry_count, error_msg, created_at, started_at, completed_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
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
	)
	if err != nil {
		return fmt.Errorf("store.create: %w", err)
	}
	return nil
}

// Get retrieves a job by ID.
func (s *Store) Get(ctx context.Context, id string) (*SpawnJob, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, status, priority, agent_type, session_name, batch_id, dispatch_id, spawn_opts, max_retries, retry_count, error_msg, created_at, started_at, completed_at
		 FROM scheduler_jobs WHERE id = ?`, id)

	return scanJob(row)
}

// Update updates status, dispatch_id, retry_count, error_msg, started_at, and completed_at.
func (s *Store) Update(ctx context.Context, job *SpawnJob) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE scheduler_jobs SET status=?, dispatch_id=?, retry_count=?, error_msg=?, started_at=?, completed_at=? WHERE id=?`,
		string(job.Status),
		nullString(job.DispatchID),
		job.RetryCount,
		nullString(job.Error),
		nullUnix(job.StartedAt),
		nullUnix(job.CompletedAt),
		job.ID,
	)
	if err != nil {
		return fmt.Errorf("store.update: %w", err)
	}
	return nil
}

// List returns jobs matching the given status filter (empty = all).
func (s *Store) List(ctx context.Context, status string, limit int) ([]*SpawnJob, error) {
	var query string
	var args []any

	if status != "" {
		query = `SELECT id, status, priority, agent_type, session_name, batch_id, dispatch_id, spawn_opts, max_retries, retry_count, error_msg, created_at, started_at, completed_at
		         FROM scheduler_jobs WHERE status = ? ORDER BY created_at DESC LIMIT ?`
		args = []any{status, limit}
	} else {
		query = `SELECT id, status, priority, agent_type, session_name, batch_id, dispatch_id, spawn_opts, max_retries, retry_count, error_msg, created_at, started_at, completed_at
		         FROM scheduler_jobs ORDER BY created_at DESC LIMIT ?`
		args = []any{limit}
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store.list: %w", err)
	}
	defer rows.Close()

	var jobs []*SpawnJob
	for rows.Next() {
		job, err := scanJobFromRows(rows)
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

// RecoverPending returns all jobs in pending or running status (for crash recovery).
func (s *Store) RecoverPending(ctx context.Context) ([]*SpawnJob, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, status, priority, agent_type, session_name, batch_id, dispatch_id, spawn_opts, max_retries, retry_count, error_msg, created_at, started_at, completed_at
		 FROM scheduler_jobs WHERE status IN ('pending', 'running', 'scheduled', 'retrying')
		 ORDER BY priority ASC, created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("store.recover: %w", err)
	}
	defer rows.Close()

	var jobs []*SpawnJob
	for rows.Next() {
		job, err := scanJobFromRows(rows)
		if err != nil {
			return nil, fmt.Errorf("store.recover: scan: %w", err)
		}
		// Reset running jobs to pending (they were interrupted).
		if job.Status == StatusRunning || job.Status == StatusScheduled {
			job.Status = StatusPending
			job.StartedAt = time.Time{}
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.recover: rows: %w", err)
	}
	return jobs, nil
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

// scanJob scans a single row into a SpawnJob.
func scanJob(row *sql.Row) (*SpawnJob, error) {
	var (
		j           SpawnJob
		status      string
		priority    int
		sessionName sql.NullString
		batchID     sql.NullString
		dispatchID  sql.NullString
		errorMsg    sql.NullString
		createdAt   int64
		startedAt   sql.NullInt64
		completedAt sql.NullInt64
	)

	err := row.Scan(
		&j.ID, &status, &priority, &j.AgentType,
		&sessionName, &batchID, &dispatchID, &j.SpawnOpts,
		&j.MaxRetries, &j.RetryCount, &errorMsg,
		&createdAt, &startedAt, &completedAt,
	)
	if err != nil {
		return nil, err
	}

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

	return &j, nil
}

// scanJobFromRows scans from *sql.Rows.
func scanJobFromRows(rows *sql.Rows) (*SpawnJob, error) {
	var (
		j           SpawnJob
		status      string
		priority    int
		sessionName sql.NullString
		batchID     sql.NullString
		dispatchID  sql.NullString
		errorMsg    sql.NullString
		createdAt   int64
		startedAt   sql.NullInt64
		completedAt sql.NullInt64
	)

	err := rows.Scan(
		&j.ID, &status, &priority, &j.AgentType,
		&sessionName, &batchID, &dispatchID, &j.SpawnOpts,
		&j.MaxRetries, &j.RetryCount, &errorMsg,
		&createdAt, &startedAt, &completedAt,
	)
	if err != nil {
		return nil, err
	}

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

	return &j, nil
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
