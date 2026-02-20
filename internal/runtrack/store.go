package runtrack

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
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

// UpdateAgentDispatch sets the dispatch_id on an agent record.
// Uses CAS semantics: only succeeds if dispatch_id is currently NULL.
// Returns ErrDispatchIDConflict if the agent already has a dispatch_id set.
func (s *Store) UpdateAgentDispatch(ctx context.Context, agentID, dispatchID string) error {
	now := time.Now().Unix()
	result, err := s.db.ExecContext(ctx, `
		UPDATE run_agents SET dispatch_id = ?, updated_at = ?
		WHERE id = ? AND dispatch_id IS NULL`,
		dispatchID, now, agentID,
	)
	if err != nil {
		return fmt.Errorf("agent update dispatch: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("agent update dispatch: %w", err)
	}
	if n == 0 {
		// Distinguish: agent not found vs. dispatch_id already set
		_, err := s.GetAgent(ctx, agentID)
		if err != nil {
			return ErrAgentNotFound
		}
		return ErrDispatchIDConflict
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

// ListPendingAgentIDs returns agent IDs with status='active' for a run.
func (s *Store) ListPendingAgentIDs(ctx context.Context, runID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM run_agents WHERE run_id = ? AND status = 'active'`, runID)
	if err != nil {
		return nil, fmt.Errorf("list pending agents: %w", err)
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

// --- Artifact operations ---

// AddArtifact inserts a new artifact record for a run. Returns the artifact ID.
// If the artifact Path points to an existing file and ContentHash is not set,
// the file's SHA256 hash is computed automatically.
func (s *Store) AddArtifact(ctx context.Context, a *Artifact) (string, error) {
	id, err := generateID()
	if err != nil {
		return "", err
	}

	// Compute content hash if file exists and no explicit hash was provided.
	var contentHash *string
	if a.ContentHash != nil {
		contentHash = a.ContentHash
	} else if a.Path != "" {
		if h, err := hashFile(a.Path); err == nil {
			contentHash = &h
		}
		// If file doesn't exist or can't be read, contentHash stays nil.
	}

	now := time.Now().Unix()
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO run_artifacts (
			id, run_id, phase, path, type, content_hash, dispatch_id, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, a.RunID, a.Phase, a.Path, a.Type, contentHash, a.DispatchID, now,
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
		query = `SELECT id, run_id, phase, path, type, content_hash, dispatch_id, status, created_at
			FROM run_artifacts WHERE run_id = ? AND phase = ? ORDER BY created_at ASC`
		args = []interface{}{runID, *phase}
	} else {
		query = `SELECT id, run_id, phase, path, type, content_hash, dispatch_id, status, created_at
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
		var (
			contentHash sql.NullString
			dispatchID  sql.NullString
			status      sql.NullString
		)
		if err := rows.Scan(
			&a.ID, &a.RunID, &a.Phase, &a.Path, &a.Type,
			&contentHash, &dispatchID, &status, &a.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("artifact list scan: %w", err)
		}
		a.ContentHash = nullStr(contentHash)
		a.DispatchID = nullStr(dispatchID)
		a.Status = nullStr(status)
		artifacts = append(artifacts, a)
	}
	return artifacts, rows.Err()
}

// MarkArtifactsRolledBack sets status='rolled_back' on all active artifacts in the given phases.
// Returns the number of artifacts marked.
func (s *Store) MarkArtifactsRolledBack(ctx context.Context, runID string, phases []string) (int64, error) {
	if len(phases) == 0 {
		return 0, nil
	}

	placeholders := make([]string, len(phases))
	args := make([]interface{}, 0, len(phases)+1)
	args = append(args, runID)
	for i, p := range phases {
		placeholders[i] = "?"
		args = append(args, p)
	}

	query := fmt.Sprintf(
		"UPDATE run_artifacts SET status = 'rolled_back' WHERE run_id = ? AND status = 'active' AND phase IN (%s)",
		strings.Join(placeholders, ", "),
	)
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("mark artifacts rolled back: %w", err)
	}
	return result.RowsAffected()
}

// FailAgentsByRun sets status='failed' on all active agents for a run.
// Returns the number of agents updated.
func (s *Store) FailAgentsByRun(ctx context.Context, runID string) (int64, error) {
	now := time.Now().Unix()
	result, err := s.db.ExecContext(ctx,
		"UPDATE run_agents SET status = ?, updated_at = ? WHERE run_id = ? AND status = ?",
		StatusFailed, now, runID, StatusActive,
	)
	if err != nil {
		return 0, fmt.Errorf("fail agents by run: %w", err)
	}
	return result.RowsAffected()
}

// --- Gate query methods (satisfy phase.RuntrackQuerier) ---

// CountArtifacts returns the number of active artifacts for a run in the given phase.
// Rolled-back artifacts are excluded from the count.
func (s *Store) CountArtifacts(ctx context.Context, runID, phase string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM run_artifacts WHERE run_id = ? AND phase = ? AND status = 'active'`,
		runID, phase).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count artifacts: %w", err)
	}
	return count, nil
}

// CountActiveAgents returns the number of agents with status='active' for a run.
func (s *Store) CountActiveAgents(ctx context.Context, runID string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM run_agents WHERE run_id = ? AND status = 'active'`,
		runID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count active agents: %w", err)
	}
	return count, nil
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

// hashFile computes the SHA256 hash of a file, returning "sha256:<hex>".
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("sha256:%x", h.Sum(nil)), nil
}
