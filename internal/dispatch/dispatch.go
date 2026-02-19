package dispatch

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

var ErrNotFound = errors.New("dispatch not found")

// Status constants for the dispatch lifecycle.
const (
	StatusSpawned   = "spawned"
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
	StatusTimeout   = "timeout"
	StatusCancelled = "cancelled"
)

// Dispatch represents a tracked agent dispatch.
type Dispatch struct {
	ID            string
	AgentType     string
	Status        string
	ProjectDir    string
	PromptFile    *string
	PromptHash    *string
	OutputFile    *string
	VerdictFile   *string
	PID           *int
	ExitCode      *int
	Name          *string
	Model         *string
	Sandbox       *string
	TimeoutSec    *int
	Turns         int
	Commands      int
	Messages      int
	InputTokens   int
	OutputTokens  int
	CreatedAt     int64
	StartedAt     *int64
	CompletedAt   *int64
	VerdictStatus *string
	VerdictSummary *string
	ErrorMessage  *string
	ScopeID       *string
	ParentID      *string
}

// IsTerminal returns true if the dispatch is in a final state.
func (d *Dispatch) IsTerminal() bool {
	switch d.Status {
	case StatusCompleted, StatusFailed, StatusTimeout, StatusCancelled:
		return true
	}
	return false
}

// Store provides dispatch operations against the intercore DB.
type Store struct {
	db *sql.DB
}

// New creates a dispatch store.
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

// Create inserts a new dispatch record and returns its ID.
func (s *Store) Create(ctx context.Context, d *Dispatch) (string, error) {
	id, err := generateID()
	if err != nil {
		return "", err
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO dispatches (
			id, agent_type, status, project_dir, prompt_file, prompt_hash,
			output_file, verdict_file, name, model, sandbox, timeout_sec,
			scope_id, parent_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, d.AgentType, StatusSpawned, d.ProjectDir,
		d.PromptFile, d.PromptHash, d.OutputFile, d.VerdictFile,
		d.Name, d.Model, d.Sandbox, d.TimeoutSec,
		d.ScopeID, d.ParentID,
	)
	if err != nil {
		return "", fmt.Errorf("dispatch create: %w", err)
	}

	return id, nil
}

// Get retrieves a dispatch by ID.
func (s *Store) Get(ctx context.Context, id string) (*Dispatch, error) {
	d := &Dispatch{}
	var (
		promptFile     sql.NullString
		promptHash     sql.NullString
		outputFile     sql.NullString
		verdictFile    sql.NullString
		pid            sql.NullInt64
		exitCode       sql.NullInt64
		name           sql.NullString
		model          sql.NullString
		sandbox        sql.NullString
		timeoutSec     sql.NullInt64
		startedAt      sql.NullInt64
		completedAt    sql.NullInt64
		verdictStatus  sql.NullString
		verdictSummary sql.NullString
		errorMessage   sql.NullString
		scopeID        sql.NullString
		parentID       sql.NullString
	)

	err := s.db.QueryRowContext(ctx, `
		SELECT id, agent_type, status, project_dir, prompt_file, prompt_hash,
			output_file, verdict_file, pid, exit_code, name, model, sandbox,
			timeout_sec, turns, commands, messages, input_tokens, output_tokens,
			created_at, started_at, completed_at, verdict_status, verdict_summary,
			error_message, scope_id, parent_id
		FROM dispatches WHERE id = ?`, id).Scan(
		&d.ID, &d.AgentType, &d.Status, &d.ProjectDir,
		&promptFile, &promptHash, &outputFile, &verdictFile,
		&pid, &exitCode, &name, &model, &sandbox,
		&timeoutSec, &d.Turns, &d.Commands, &d.Messages,
		&d.InputTokens, &d.OutputTokens,
		&d.CreatedAt, &startedAt, &completedAt,
		&verdictStatus, &verdictSummary, &errorMessage,
		&scopeID, &parentID,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("dispatch get: %w", err)
	}

	d.PromptFile = nullStr(promptFile)
	d.PromptHash = nullStr(promptHash)
	d.OutputFile = nullStr(outputFile)
	d.VerdictFile = nullStr(verdictFile)
	d.PID = nullInt(pid)
	d.ExitCode = nullInt(exitCode)
	d.Name = nullStr(name)
	d.Model = nullStr(model)
	d.Sandbox = nullStr(sandbox)
	d.TimeoutSec = nullInt(timeoutSec)
	d.StartedAt = nullInt64(startedAt)
	d.CompletedAt = nullInt64(completedAt)
	d.VerdictStatus = nullStr(verdictStatus)
	d.VerdictSummary = nullStr(verdictSummary)
	d.ErrorMessage = nullStr(errorMessage)
	d.ScopeID = nullStr(scopeID)
	d.ParentID = nullStr(parentID)

	return d, nil
}

// UpdateFields is a map of column names to values for partial updates.
type UpdateFields map[string]interface{}

// UpdateStatus transitions a dispatch to a new status with optional field updates.
func (s *Store) UpdateStatus(ctx context.Context, id, status string, fields UpdateFields) error {
	// Build dynamic SET clause
	sets := []string{"status = ?"}
	args := []interface{}{status}

	for col, val := range fields {
		sets[0] = sets[0] // keep status first
		sets = append(sets, col+" = ?")
		args = append(args, val)
	}
	args = append(args, id)

	query := "UPDATE dispatches SET " + joinStrings(sets, ", ") + " WHERE id = ?"
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("dispatch update: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("dispatch update: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListActive returns all non-terminal dispatches.
func (s *Store) ListActive(ctx context.Context) ([]*Dispatch, error) {
	return s.queryDispatches(ctx,
		"SELECT "+dispatchCols+" FROM dispatches WHERE status IN ('spawned', 'running') ORDER BY created_at DESC")
}

// List returns dispatches with optional scope filter.
func (s *Store) List(ctx context.Context, scopeID *string) ([]*Dispatch, error) {
	if scopeID != nil {
		return s.queryDispatches(ctx,
			"SELECT "+dispatchCols+" FROM dispatches WHERE scope_id = ? ORDER BY created_at DESC", *scopeID)
	}
	return s.queryDispatches(ctx,
		"SELECT "+dispatchCols+" FROM dispatches ORDER BY created_at DESC")
}

// Prune deletes dispatches older than the given duration.
func (s *Store) Prune(ctx context.Context, olderThan time.Duration) (int64, error) {
	threshold := time.Now().Unix() - int64(olderThan.Seconds())
	result, err := s.db.ExecContext(ctx,
		"DELETE FROM dispatches WHERE created_at < ? AND status NOT IN ('spawned', 'running')",
		threshold)
	if err != nil {
		return 0, fmt.Errorf("dispatch prune: %w", err)
	}
	return result.RowsAffected()
}

// --- Gate query methods (satisfy phase.VerdictQuerier) ---

// HasVerdict returns true if any dispatch for the given scope has a non-null, non-reject verdict.
// When scopeID is empty, returns false (gate fails explicitly — use override for unusual configs).
func (s *Store) HasVerdict(ctx context.Context, scopeID string) (bool, error) {
	if scopeID == "" {
		return false, nil
	}
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM dispatches
			WHERE scope_id = ? AND verdict_status IS NOT NULL AND verdict_status != 'reject'`,
		scopeID).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("has verdict: %w", err)
	}
	return count > 0, nil
}

// --- helpers ---

const dispatchCols = `id, agent_type, status, project_dir, prompt_file, prompt_hash,
	output_file, verdict_file, pid, exit_code, name, model, sandbox,
	timeout_sec, turns, commands, messages, input_tokens, output_tokens,
	created_at, started_at, completed_at, verdict_status, verdict_summary,
	error_message, scope_id, parent_id`

func (s *Store) queryDispatches(ctx context.Context, query string, args ...interface{}) ([]*Dispatch, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("dispatch list: %w", err)
	}
	defer rows.Close()

	var dispatches []*Dispatch
	for rows.Next() {
		d := &Dispatch{}
		var (
			promptFile     sql.NullString
			promptHash     sql.NullString
			outputFile     sql.NullString
			verdictFile    sql.NullString
			pid            sql.NullInt64
			exitCode       sql.NullInt64
			name           sql.NullString
			model          sql.NullString
			sandbox        sql.NullString
			timeoutSec     sql.NullInt64
			startedAt      sql.NullInt64
			completedAt    sql.NullInt64
			verdictStatus  sql.NullString
			verdictSummary sql.NullString
			errorMessage   sql.NullString
			scopeID        sql.NullString
			parentID       sql.NullString
		)

		if err := rows.Scan(
			&d.ID, &d.AgentType, &d.Status, &d.ProjectDir,
			&promptFile, &promptHash, &outputFile, &verdictFile,
			&pid, &exitCode, &name, &model, &sandbox,
			&timeoutSec, &d.Turns, &d.Commands, &d.Messages,
			&d.InputTokens, &d.OutputTokens,
			&d.CreatedAt, &startedAt, &completedAt,
			&verdictStatus, &verdictSummary, &errorMessage,
			&scopeID, &parentID,
		); err != nil {
			return nil, fmt.Errorf("dispatch list scan: %w", err)
		}

		d.PromptFile = nullStr(promptFile)
		d.PromptHash = nullStr(promptHash)
		d.OutputFile = nullStr(outputFile)
		d.VerdictFile = nullStr(verdictFile)
		d.PID = nullInt(pid)
		d.ExitCode = nullInt(exitCode)
		d.Name = nullStr(name)
		d.Model = nullStr(model)
		d.Sandbox = nullStr(sandbox)
		d.TimeoutSec = nullInt(timeoutSec)
		d.StartedAt = nullInt64(startedAt)
		d.CompletedAt = nullInt64(completedAt)
		d.VerdictStatus = nullStr(verdictStatus)
		d.VerdictSummary = nullStr(verdictSummary)
		d.ErrorMessage = nullStr(errorMessage)
		d.ScopeID = nullStr(scopeID)
		d.ParentID = nullStr(parentID)

		dispatches = append(dispatches, d)
	}
	return dispatches, rows.Err()
}

func nullStr(ns sql.NullString) *string {
	if ns.Valid {
		return &ns.String
	}
	return nil
}

func nullInt(ni sql.NullInt64) *int {
	if ni.Valid {
		v := int(ni.Int64)
		return &v
	}
	return nil
}

func nullInt64(ni sql.NullInt64) *int64 {
	if ni.Valid {
		return &ni.Int64
	}
	return nil
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
