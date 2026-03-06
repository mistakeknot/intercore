package session

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Session represents a registered agent session.
type Session struct {
	ID         int64   `json:"id"`
	SessionID  string  `json:"session_id"`
	ProjectDir string  `json:"project_dir"`
	AgentType  string  `json:"agent_type"`
	Model      *string `json:"model,omitempty"`
	StartedAt  int64   `json:"started_at"`
	EndedAt    *int64  `json:"ended_at,omitempty"`
	Metadata   *string `json:"metadata,omitempty"`
}

// Attribution represents a point-in-time attribution change within a session.
type Attribution struct {
	ID         int64   `json:"id"`
	SessionID  string  `json:"session_id"`
	ProjectDir string  `json:"project_dir"`
	BeadID     *string `json:"bead_id,omitempty"`
	RunID      *string `json:"run_id,omitempty"`
	Phase      *string `json:"phase,omitempty"`
	CreatedAt  int64   `json:"created_at"`
}

// CurrentAttribution is the latest attribution state for a session.
type CurrentAttribution struct {
	SessionID  string  `json:"session_id"`
	ProjectDir string  `json:"project_dir"`
	BeadID     *string `json:"bead_id,omitempty"`
	RunID      *string `json:"run_id,omitempty"`
	Phase      *string `json:"phase,omitempty"`
	UpdatedAt  int64   `json:"updated_at"`
}

// StartOpts holds the fields for registering a session.
type StartOpts struct {
	SessionID  string
	ProjectDir string
	AgentType  string
	Model      string
	Metadata   string
}

// AttributeOpts holds the fields for recording an attribution change.
type AttributeOpts struct {
	SessionID  string
	ProjectDir string
	BeadID     string
	RunID      string
	Phase      string
}

// ListOpts filters session queries.
type ListOpts struct {
	ProjectDir string
	SessionID  string
	Since      int64
	ActiveOnly bool
	Limit      int
}

// Store provides session operations.
type Store struct {
	db *sql.DB
}

// NewStore creates a session store.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// Start registers or re-registers a session. Returns the row ID.
// Idempotent: if the (session_id, project_dir) pair already exists,
// the existing row is updated with the new metadata/model/agent_type.
func (s *Store) Start(ctx context.Context, opts StartOpts) (int64, error) {
	agentType := opts.AgentType
	if agentType == "" {
		agentType = "claude-code"
	}

	now := time.Now().Unix()

	result, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (session_id, project_dir, agent_type, model, metadata, started_at)
		VALUES (?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?)
		ON CONFLICT(session_id, project_dir) DO UPDATE SET
			agent_type = excluded.agent_type,
			model = COALESCE(excluded.model, sessions.model),
			metadata = COALESCE(excluded.metadata, sessions.metadata),
			started_at = excluded.started_at,
			ended_at = NULL`,
		opts.SessionID, opts.ProjectDir, agentType,
		opts.Model, opts.Metadata, now,
	)
	if err != nil {
		return 0, fmt.Errorf("session start: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("session start: last id: %w", err)
	}

	// ON CONFLICT DO UPDATE returns the existing rowid on some drivers,
	// but modernc.org/sqlite may return 0 — look up if needed.
	if id == 0 {
		row := s.db.QueryRowContext(ctx,
			`SELECT id FROM sessions WHERE session_id = ? AND project_dir = ?`,
			opts.SessionID, opts.ProjectDir,
		)
		if err := row.Scan(&id); err != nil {
			return 0, fmt.Errorf("session start: lookup existing: %w", err)
		}
	}

	return id, nil
}

// Attribute records an attribution change within a session. Returns the row ID.
// Only non-empty fields are written (empty strings become NULL).
func (s *Store) Attribute(ctx context.Context, opts AttributeOpts) (int64, error) {
	if opts.SessionID == "" {
		return 0, fmt.Errorf("session attribute: session_id is required")
	}

	now := time.Now().Unix()

	result, err := s.db.ExecContext(ctx, `
		INSERT INTO session_attributions (session_id, project_dir, bead_id, run_id, phase, created_at)
		VALUES (?, ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), ?)`,
		opts.SessionID, opts.ProjectDir,
		opts.BeadID, opts.RunID, opts.Phase,
		now,
	)
	if err != nil {
		return 0, fmt.Errorf("session attribute: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("session attribute: last id: %w", err)
	}

	return id, nil
}

// End marks a session as ended. Sets ended_at on all rows matching the session_id.
func (s *Store) End(ctx context.Context, sessionID string) error {
	now := time.Now().Unix()

	result, err := s.db.ExecContext(ctx, `
		UPDATE sessions SET ended_at = ?
		WHERE session_id = ? AND ended_at IS NULL`,
		now, sessionID,
	)
	if err != nil {
		return fmt.Errorf("session end: %w", err)
	}

	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("session end: no active session with id %q", sessionID)
	}
	return nil
}

// Current returns the latest attribution for a session in a project.
// Returns nil if no attributions exist.
func (s *Store) Current(ctx context.Context, sessionID, projectDir string) (*CurrentAttribution, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT session_id, project_dir, bead_id, run_id, phase, created_at
		FROM session_attributions
		WHERE session_id = ? AND project_dir = ?
		ORDER BY created_at DESC, id DESC
		LIMIT 1`,
		sessionID, projectDir,
	)

	var ca CurrentAttribution
	var beadID, runID, phase sql.NullString

	if err := row.Scan(&ca.SessionID, &ca.ProjectDir, &beadID, &runID, &phase, &ca.UpdatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("session current: %w", err)
	}

	ca.BeadID = nullStr(beadID)
	ca.RunID = nullStr(runID)
	ca.Phase = nullStr(phase)

	return &ca, nil
}

// List returns sessions matching the given filters.
func (s *Store) List(ctx context.Context, opts ListOpts) ([]Session, error) {
	var where []string
	var args []interface{}

	if opts.ProjectDir != "" {
		where = append(where, "project_dir = ?")
		args = append(args, opts.ProjectDir)
	}
	if opts.SessionID != "" {
		where = append(where, "session_id = ?")
		args = append(args, opts.SessionID)
	}
	if opts.Since > 0 {
		where = append(where, "started_at >= ?")
		args = append(args, opts.Since)
	}
	if opts.ActiveOnly {
		where = append(where, "ended_at IS NULL")
	}

	query := "SELECT id, session_id, project_dir, agent_type, model, started_at, ended_at, metadata FROM sessions"
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY started_at DESC"

	limit := opts.Limit
	if limit <= 0 {
		limit = 1000
	}
	query += fmt.Sprintf(" LIMIT %d", limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("session list: %w", err)
	}
	defer rows.Close()

	var sessions []Session
	for rows.Next() {
		var sess Session
		var model, metadata sql.NullString
		var endedAt sql.NullInt64

		if err := rows.Scan(
			&sess.ID, &sess.SessionID, &sess.ProjectDir, &sess.AgentType,
			&model, &sess.StartedAt, &endedAt, &metadata,
		); err != nil {
			return nil, fmt.Errorf("session list: scan: %w", err)
		}

		sess.Model = nullStr(model)
		sess.EndedAt = nullInt64Ptr(endedAt)
		sess.Metadata = nullStr(metadata)

		sessions = append(sessions, sess)
	}
	return sessions, rows.Err()
}

func nullStr(ns sql.NullString) *string {
	if !ns.Valid {
		return nil
	}
	return &ns.String
}

func nullInt64Ptr(ni sql.NullInt64) *int64 {
	if !ni.Valid {
		return nil
	}
	return &ni.Int64
}
