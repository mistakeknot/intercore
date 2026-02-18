package phase

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"time"
)

const idChars = "abcdefghijklmnopqrstuvwxyz0123456789"
const idLen = 8

// Store provides run + phase_event operations against the intercore DB.
type Store struct {
	db *sql.DB
}

// New creates a phase store.
func New(db *sql.DB) *Store {
	return &Store{db: db}
}

// generateID creates an 8-char random alphanumeric ID.
func generateID() (string, error) {
	b := make([]byte, idLen)
	max := big.NewInt(int64(len(idChars)))
	for i := range b {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", fmt.Errorf("generate id: %w", err)
		}
		b[i] = idChars[n.Int64()]
	}
	return string(b), nil
}

// Create inserts a new run and returns its ID.
func (s *Store) Create(ctx context.Context, r *Run) (string, error) {
	id, err := generateID()
	if err != nil {
		return "", err
	}

	now := time.Now().Unix()
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO runs (
			id, project_dir, goal, status, phase, complexity,
			force_full, auto_advance, created_at, updated_at,
			scope_id, metadata
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, r.ProjectDir, r.Goal, StatusActive, PhaseBrainstorm,
		r.Complexity, boolToInt(r.ForceFull), boolToInt(r.AutoAdvance),
		now, now, r.ScopeID, r.Metadata,
	)
	if err != nil {
		return "", fmt.Errorf("run create: %w", err)
	}
	return id, nil
}

// Get retrieves a run by ID.
func (s *Store) Get(ctx context.Context, id string) (*Run, error) {
	r := &Run{}
	var (
		completedAt sql.NullInt64
		scopeID     sql.NullString
		metadata    sql.NullString
		forceFull   int
		autoAdvance int
	)

	err := s.db.QueryRowContext(ctx, `
		SELECT id, project_dir, goal, status, phase, complexity,
			force_full, auto_advance, created_at, updated_at,
			completed_at, scope_id, metadata
		FROM runs WHERE id = ?`, id).Scan(
		&r.ID, &r.ProjectDir, &r.Goal, &r.Status, &r.Phase,
		&r.Complexity, &forceFull, &autoAdvance,
		&r.CreatedAt, &r.UpdatedAt,
		&completedAt, &scopeID, &metadata,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("run get: %w", err)
	}

	r.ForceFull = forceFull != 0
	r.AutoAdvance = autoAdvance != 0
	r.CompletedAt = nullInt64(completedAt)
	r.ScopeID = nullStr(scopeID)
	r.Metadata = nullStr(metadata)

	return r, nil
}

// UpdatePhase transitions a run's phase with optimistic concurrency.
// Returns ErrStalePhase if the run's current phase doesn't match expectedPhase.
func (s *Store) UpdatePhase(ctx context.Context, id, expectedPhase, newPhase string) error {
	now := time.Now().Unix()
	result, err := s.db.ExecContext(ctx, `
		UPDATE runs SET phase = ?, updated_at = ?
		WHERE id = ? AND phase = ?`,
		newPhase, now, id, expectedPhase,
	)
	if err != nil {
		return fmt.Errorf("run update phase: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("run update phase: %w", err)
	}
	if n == 0 {
		// Distinguish not-found from stale phase
		_, getErr := s.Get(ctx, id)
		if getErr != nil {
			return ErrNotFound
		}
		return ErrStalePhase
	}
	return nil
}

// UpdateStatus sets a run's status (e.g., completed, cancelled).
func (s *Store) UpdateStatus(ctx context.Context, id, status string) error {
	now := time.Now().Unix()
	var completedAt *int64
	if IsTerminalStatus(status) {
		completedAt = &now
	}

	result, err := s.db.ExecContext(ctx, `
		UPDATE runs SET status = ?, updated_at = ?, completed_at = COALESCE(?, completed_at)
		WHERE id = ?`,
		status, now, completedAt, id,
	)
	if err != nil {
		return fmt.Errorf("run update status: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("run update status: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateSettings updates run configuration fields.
func (s *Store) UpdateSettings(ctx context.Context, id string, complexity *int, autoAdvance *bool, forceFull *bool) error {
	now := time.Now().Unix()

	sets := []string{"updated_at = ?"}
	args := []interface{}{now}

	if complexity != nil {
		sets = append(sets, "complexity = ?")
		args = append(args, *complexity)
	}
	if autoAdvance != nil {
		sets = append(sets, "auto_advance = ?")
		args = append(args, boolToInt(*autoAdvance))
	}
	if forceFull != nil {
		sets = append(sets, "force_full = ?")
		args = append(args, boolToInt(*forceFull))
	}

	args = append(args, id)
	query := "UPDATE runs SET " + joinStrings(sets, ", ") + " WHERE id = ?"

	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("run update settings: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("run update settings: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListActive returns all runs with status='active'.
func (s *Store) ListActive(ctx context.Context) ([]*Run, error) {
	return s.queryRuns(ctx,
		"SELECT "+runCols+" FROM runs WHERE status = 'active' ORDER BY created_at DESC")
}

// List returns runs, optionally filtered by scope_id.
func (s *Store) List(ctx context.Context, scopeID *string) ([]*Run, error) {
	if scopeID != nil {
		return s.queryRuns(ctx,
			"SELECT "+runCols+" FROM runs WHERE scope_id = ? ORDER BY created_at DESC", *scopeID)
	}
	return s.queryRuns(ctx,
		"SELECT "+runCols+" FROM runs ORDER BY created_at DESC")
}

// AddEvent inserts a phase event audit record.
func (s *Store) AddEvent(ctx context.Context, e *PhaseEvent) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO phase_events (
			run_id, from_phase, to_phase, event_type,
			gate_result, gate_tier, reason
		) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		e.RunID, e.FromPhase, e.ToPhase, e.EventType,
		e.GateResult, e.GateTier, e.Reason,
	)
	if err != nil {
		return fmt.Errorf("event add: %w", err)
	}
	return nil
}

// Events returns all phase events for a run, ordered by id ASC.
func (s *Store) Events(ctx context.Context, runID string) ([]*PhaseEvent, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, run_id, from_phase, to_phase, event_type,
			gate_result, gate_tier, reason, created_at
		FROM phase_events WHERE run_id = ? ORDER BY id ASC`, runID)
	if err != nil {
		return nil, fmt.Errorf("events list: %w", err)
	}
	defer rows.Close()

	var events []*PhaseEvent
	for rows.Next() {
		e := &PhaseEvent{}
		var (
			gateResult sql.NullString
			gateTier   sql.NullString
			reason     sql.NullString
		)
		if err := rows.Scan(
			&e.ID, &e.RunID, &e.FromPhase, &e.ToPhase, &e.EventType,
			&gateResult, &gateTier, &reason, &e.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("events scan: %w", err)
		}
		e.GateResult = nullStr(gateResult)
		e.GateTier = nullStr(gateTier)
		e.Reason = nullStr(reason)
		events = append(events, e)
	}
	return events, rows.Err()
}

// --- helpers ---

const runCols = `id, project_dir, goal, status, phase, complexity,
	force_full, auto_advance, created_at, updated_at,
	completed_at, scope_id, metadata`

func (s *Store) queryRuns(ctx context.Context, query string, args ...interface{}) ([]*Run, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("run list: %w", err)
	}
	defer rows.Close()

	var runs []*Run
	for rows.Next() {
		r := &Run{}
		var (
			completedAt sql.NullInt64
			scopeID     sql.NullString
			metadata    sql.NullString
			forceFull   int
			autoAdvance int
		)
		if err := rows.Scan(
			&r.ID, &r.ProjectDir, &r.Goal, &r.Status, &r.Phase,
			&r.Complexity, &forceFull, &autoAdvance,
			&r.CreatedAt, &r.UpdatedAt,
			&completedAt, &scopeID, &metadata,
		); err != nil {
			return nil, fmt.Errorf("run list scan: %w", err)
		}
		r.ForceFull = forceFull != 0
		r.AutoAdvance = autoAdvance != 0
		r.CompletedAt = nullInt64(completedAt)
		r.ScopeID = nullStr(scopeID)
		r.Metadata = nullStr(metadata)
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

func nullStr(ns sql.NullString) *string {
	if ns.Valid {
		return &ns.String
	}
	return nil
}

func nullInt64(ni sql.NullInt64) *int64 {
	if ni.Valid {
		return &ni.Int64
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func joinStrings(ss []string, sep string) string {
	if len(ss) == 0 {
		return ""
	}
	result := ss[0]
	for _, s := range ss[1:] {
		result += sep + s
	}
	return result
}
