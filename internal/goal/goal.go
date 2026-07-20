// Package goal implements the first-class Goal entity: a durable unit of
// intent that contains runs, carries a machine-evaluable completion
// condition, and closes through a fenced, per-step terminal sequence.
// Design: docs/brainstorms/2026-07-18-goal-native-cycle-brainstorm.md KD 1/3/8.
package goal

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

var (
	ErrNotFound        = errors.New("goal: not found")
	ErrLeaseHeld       = errors.New("goal: close lease held by another owner")
	ErrStaleFence      = errors.New("goal: stale fence — lease was broken and reacquired")
	ErrCloseIncomplete = errors.New("goal: terminal steps incomplete")
)

var nowUnix = func() int64 { return time.Now().Unix() }

// Goal is one row of the goals table. Timestamps are unix seconds.
type Goal struct {
	ID                  string
	ProjectDir          string
	Title               string
	CharterPath         *string
	ConditionText       string
	Status              string
	Complexity          int
	FenceGen            int64
	ClosingRunID        *string
	LeaseOwner          *string
	LeaseExpiresAt      *int64
	VerifiedAt          *int64
	ReflectedAt         *int64
	CompoundedAt        *int64
	SuccessorProposedAt *int64
	SuccessorRef        *string
	LastRunAdvancedAt   *int64
	BeadID              *string
	CreatedAt           int64
	UpdatedAt           int64
	AmendedAt           *int64
	ClosedAt            *int64
}

type Store struct{ db *sql.DB }

func New(db *sql.DB) *Store { return &Store{db: db} }

func newID() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

const goalCols = `id, project_dir, title, charter_path, condition_text, status,
	complexity, fence_gen, closing_run_id, lease_owner, lease_expires_at,
	verified_at, reflected_at, compounded_at, successor_proposed_at,
	successor_ref, last_run_advanced_at, bead_id, created_at, updated_at,
	amended_at, closed_at`

func scanGoal(row interface{ Scan(...any) error }) (*Goal, error) {
	var g Goal
	err := row.Scan(&g.ID, &g.ProjectDir, &g.Title, &g.CharterPath,
		&g.ConditionText, &g.Status, &g.Complexity, &g.FenceGen,
		&g.ClosingRunID, &g.LeaseOwner, &g.LeaseExpiresAt, &g.VerifiedAt,
		&g.ReflectedAt, &g.CompoundedAt, &g.SuccessorProposedAt,
		&g.SuccessorRef, &g.LastRunAdvancedAt, &g.BeadID, &g.CreatedAt,
		&g.UpdatedAt, &g.AmendedAt, &g.ClosedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &g, nil
}

func (s *Store) Create(ctx context.Context, g *Goal) (string, error) {
	id, err := newID()
	if err != nil {
		return "", fmt.Errorf("goal create: %w", err)
	}
	if g.Complexity == 0 {
		g.Complexity = 3
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO goals
		(id, project_dir, title, charter_path, condition_text, complexity, bead_id)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, g.ProjectDir, g.Title, g.CharterPath, g.ConditionText, g.Complexity, g.BeadID)
	if err != nil {
		return "", fmt.Errorf("goal create: %w", err)
	}
	return id, nil
}

func (s *Store) Get(ctx context.Context, id string) (*Goal, error) {
	return scanGoal(s.db.QueryRowContext(ctx,
		`SELECT `+goalCols+` FROM goals WHERE id = ?`, id))
}

// List returns goals for a project (empty projectDir = all projects),
// filtered by status (empty = any).
func (s *Store) List(ctx context.Context, projectDir, status string) ([]*Goal, error) {
	q := `SELECT ` + goalCols + ` FROM goals WHERE 1=1`
	var args []any
	if projectDir != "" {
		q += ` AND project_dir = ?`
		args = append(args, projectDir)
	}
	if status != "" {
		q += ` AND status = ?`
		args = append(args, status)
	}
	q += ` ORDER BY created_at DESC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Goal
	for rows.Next() {
		g, err := scanGoal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// stepCols whitelists terminal-step names to columns — never interpolate
// caller input into SQL identifiers.
var stepCols = map[string]string{
	"verified":           "verified_at",
	"reflected":          "reflected_at",
	"compounded":         "compounded_at",
	"successor_proposed": "successor_proposed_at",
}

// AcquireClose transitions open→closing (or breaks an expired closing lease)
// and returns the new fence generation. ttlSec sizes the lease to the close
// sequence's real multi-LLM-call latency — callers should renew between
// steps rather than passing a huge TTL.
func (s *Store) AcquireClose(ctx context.Context, id, runID, owner string, ttlSec int64) (int64, error) {
	now := nowUnix()
	var fence int64
	err := s.db.QueryRowContext(ctx, `UPDATE goals
		SET status = 'closing', closing_run_id = ?, lease_owner = ?,
		    lease_expires_at = ?, fence_gen = fence_gen + 1, updated_at = ?
		WHERE id = ?
		  AND (status = 'open'
		       OR (status = 'closing' AND lease_expires_at IS NOT NULL AND lease_expires_at < ?))
		RETURNING fence_gen`,
		runID, owner, now+ttlSec, now, id, now).Scan(&fence)
	if errors.Is(err, sql.ErrNoRows) {
		if _, gerr := s.Get(ctx, id); errors.Is(gerr, ErrNotFound) {
			return 0, ErrNotFound
		}
		return 0, ErrLeaseHeld
	}
	if err != nil {
		return 0, fmt.Errorf("goal acquire-close: %w", err)
	}
	return fence, nil
}

// RenewLease extends the lease under the current fence.
func (s *Store) RenewLease(ctx context.Context, id string, fence, ttlSec int64) error {
	now := nowUnix()
	res, err := s.db.ExecContext(ctx, `UPDATE goals
		SET lease_expires_at = ?, updated_at = ?
		WHERE id = ? AND fence_gen = ? AND status = 'closing'`,
		now+ttlSec, now, id, fence)
	if err != nil {
		return fmt.Errorf("goal renew: %w", err)
	}
	return staleUnlessOneRow(res)
}

// StampStep records one terminal step under the fence.
func (s *Store) StampStep(ctx context.Context, id, step string, fence int64) error {
	col, ok := stepCols[step]
	if !ok {
		return fmt.Errorf("goal stamp: unknown step %q", step)
	}
	now := nowUnix()
	res, err := s.db.ExecContext(ctx, `UPDATE goals
		SET `+col+` = ?, updated_at = ?
		WHERE id = ? AND fence_gen = ? AND status = 'closing'`,
		now, now, id, fence)
	if err != nil {
		return fmt.Errorf("goal stamp %s: %w", step, err)
	}
	return staleUnlessOneRow(res)
}

// FinishClose completes the goal iff every step is stamped, under the fence.
func (s *Store) FinishClose(ctx context.Context, id string, fence int64) error {
	now := nowUnix()
	res, err := s.db.ExecContext(ctx, `UPDATE goals
		SET status = 'closed', closed_at = ?, updated_at = ?,
		    lease_owner = NULL, lease_expires_at = NULL
		WHERE id = ? AND fence_gen = ? AND status = 'closing'
		  AND verified_at IS NOT NULL AND reflected_at IS NOT NULL
		  AND compounded_at IS NOT NULL AND successor_proposed_at IS NOT NULL`,
		now, now, id, fence)
	if err != nil {
		return fmt.Errorf("goal finish-close: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 1 {
		return nil
	}
	// Distinguish stale fence from incomplete steps for the caller.
	g, gerr := s.Get(ctx, id)
	if gerr != nil {
		return gerr
	}
	if g.Status == "closing" && g.FenceGen == fence {
		return ErrCloseIncomplete
	}
	return ErrStaleFence
}

// ReleaseLease abandons a close attempt (goal returns to open) under the fence.
func (s *Store) ReleaseLease(ctx context.Context, id string, fence int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE goals
		SET status = 'open', lease_owner = NULL, lease_expires_at = NULL,
		    closing_run_id = NULL, updated_at = ?
		WHERE id = ? AND fence_gen = ? AND status = 'closing'`,
		nowUnix(), id, fence)
	if err != nil {
		return fmt.Errorf("goal release: %w", err)
	}
	return staleUnlessOneRow(res)
}

func staleUnlessOneRow(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrStaleFence
	}
	return nil
}
