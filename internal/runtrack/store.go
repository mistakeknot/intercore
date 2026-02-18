package runtrack

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

const idChars = "abcdefghijklmnopqrstuvwxyz0123456789"
const idLen = 8

// Store provides run agent + artifact operations against the intercore DB.
type Store struct {
	db *sql.DB
}

// New creates a runtrack store.
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

// --- Agent operations ---

// AddAgent inserts a new agent record for a run. Returns the agent ID.
func (s *Store) AddAgent(ctx context.Context, a *Agent) (string, error) {
	id, err := generateID()
	if err != nil {
		return "", err
	}

	now := time.Now().Unix()
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO run_agents (
			id, run_id, agent_type, name, status, dispatch_id,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, a.RunID, a.AgentType, a.Name, StatusActive, a.DispatchID,
		now, now,
	)
	if err != nil {
		if isFKViolation(err) {
			return "", ErrRunNotFound
		}
		return "", fmt.Errorf("agent add: %w", err)
	}
	return id, nil
}

// UpdateAgent updates an agent's status.
func (s *Store) UpdateAgent(ctx context.Context, id, status string) error {
	now := time.Now().Unix()
	result, err := s.db.ExecContext(ctx, `
		UPDATE run_agents SET status = ?, updated_at = ? WHERE id = ?`,
		status, now, id,
	)
	if err != nil {
		return fmt.Errorf("agent update: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("agent update: %w", err)
	}
	if n == 0 {
		return ErrAgentNotFound
	}
	return nil
}

// GetAgent retrieves an agent by ID.
func (s *Store) GetAgent(ctx context.Context, id string) (*Agent, error) {
	a := &Agent{}
	var (
		name       sql.NullString
		dispatchID sql.NullString
	)

	err := s.db.QueryRowContext(ctx, `
		SELECT id, run_id, agent_type, name, status, dispatch_id,
			created_at, updated_at
		FROM run_agents WHERE id = ?`, id).Scan(
		&a.ID, &a.RunID, &a.AgentType, &name, &a.Status, &dispatchID,
		&a.CreatedAt, &a.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrAgentNotFound
		}
		return nil, fmt.Errorf("agent get: %w", err)
	}

	a.Name = nullStr(name)
	a.DispatchID = nullStr(dispatchID)
	return a, nil
}

// ListAgents returns all agents for a run, ordered by creation time.
func (s *Store) ListAgents(ctx context.Context, runID string) ([]*Agent, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, run_id, agent_type, name, status, dispatch_id,
			created_at, updated_at
		FROM run_agents WHERE run_id = ? ORDER BY created_at ASC`, runID)
	if err != nil {
		return nil, fmt.Errorf("agent list: %w", err)
	}
	defer rows.Close()

	var agents []*Agent
	for rows.Next() {
		a := &Agent{}
		var (
			name       sql.NullString
			dispatchID sql.NullString
		)
		if err := rows.Scan(
			&a.ID, &a.RunID, &a.AgentType, &name, &a.Status, &dispatchID,
			&a.CreatedAt, &a.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("agent list scan: %w", err)
		}
		a.Name = nullStr(name)
		a.DispatchID = nullStr(dispatchID)
		agents = append(agents, a)
	}
	return agents, rows.Err()
}

// --- Artifact operations ---

// AddArtifact inserts a new artifact record for a run. Returns the artifact ID.
func (s *Store) AddArtifact(ctx context.Context, a *Artifact) (string, error) {
	id, err := generateID()
	if err != nil {
		return "", err
	}

	now := time.Now().Unix()
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO run_artifacts (
			id, run_id, phase, path, type, created_at
		) VALUES (?, ?, ?, ?, ?, ?)`,
		id, a.RunID, a.Phase, a.Path, a.Type, now,
	)
	if err != nil {
		if isFKViolation(err) {
			return "", ErrRunNotFound
		}
		return "", fmt.Errorf("artifact add: %w", err)
	}
	return id, nil
}

// ListArtifacts returns all artifacts for a run, optionally filtered by phase.
func (s *Store) ListArtifacts(ctx context.Context, runID string, phase *string) ([]*Artifact, error) {
	var query string
	var args []interface{}

	if phase != nil {
		query = `SELECT id, run_id, phase, path, type, created_at
			FROM run_artifacts WHERE run_id = ? AND phase = ? ORDER BY created_at ASC`
		args = []interface{}{runID, *phase}
	} else {
		query = `SELECT id, run_id, phase, path, type, created_at
			FROM run_artifacts WHERE run_id = ? ORDER BY created_at ASC`
		args = []interface{}{runID}
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("artifact list: %w", err)
	}
	defer rows.Close()

	var artifacts []*Artifact
	for rows.Next() {
		a := &Artifact{}
		if err := rows.Scan(
			&a.ID, &a.RunID, &a.Phase, &a.Path, &a.Type, &a.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("artifact list scan: %w", err)
		}
		artifacts = append(artifacts, a)
	}
	return artifacts, rows.Err()
}

// --- helpers ---

// isFKViolation checks if an error is a SQLite foreign key constraint violation.
func isFKViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "FOREIGN KEY constraint failed")
}

func nullStr(ns sql.NullString) *string {
	if ns.Valid {
		return &ns.String
	}
	return nil
}
