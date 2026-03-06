package routing

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Decision represents a persisted routing decision record.
type Decision struct {
	ID            int64   `json:"id"`
	DispatchID    *string `json:"dispatch_id,omitempty"`
	RunID         *string `json:"run_id,omitempty"`
	SessionID     *string `json:"session_id,omitempty"`
	BeadID        *string `json:"bead_id,omitempty"`
	ProjectDir    string  `json:"project_dir"`
	Phase         *string `json:"phase,omitempty"`
	Agent         string  `json:"agent"`
	Category      *string `json:"category,omitempty"`
	SelectedModel string  `json:"selected_model"`
	RuleMatched   string  `json:"rule_matched"`
	FloorApplied  bool    `json:"floor_applied"`
	FloorFrom     *string `json:"floor_from,omitempty"`
	FloorTo       *string `json:"floor_to,omitempty"`
	Candidates    *string `json:"candidates,omitempty"`
	Excluded      *string `json:"excluded,omitempty"`
	PolicyHash    *string `json:"policy_hash,omitempty"`
	OverrideID    *string `json:"override_id,omitempty"`
	Complexity    *int    `json:"complexity,omitempty"`
	ContextJSON   *string `json:"context_json,omitempty"`
	DecidedAt     int64   `json:"decided_at"`
}

// RecordDecisionOpts holds the fields for recording a routing decision.
type RecordDecisionOpts struct {
	DispatchID    string
	RunID         string
	SessionID     string
	BeadID        string
	ProjectDir    string
	Phase         string
	Agent         string
	Category      string
	SelectedModel string
	RuleMatched   string
	FloorApplied  bool
	FloorFrom     string
	FloorTo       string
	Candidates    string // JSON array
	Excluded      string // JSON array
	PolicyHash    string
	OverrideID    string
	Complexity    int
	ContextJSON   string
}

// ListDecisionOpts filters routing decision queries.
type ListDecisionOpts struct {
	ProjectDir string
	Agent      string
	Model      string
	DispatchID string
	Since      int64
	Until      int64
	Limit      int
}

// DecisionStore provides routing decision operations.
type DecisionStore struct {
	db *sql.DB
}

// NewDecisionStore creates a routing decision store.
func NewDecisionStore(db *sql.DB) *DecisionStore {
	return &DecisionStore{db: db}
}

// Record inserts a routing decision. Returns the row ID.
func (s *DecisionStore) Record(ctx context.Context, opts RecordDecisionOpts) (int64, error) {
	now := time.Now().Unix()
	floorApplied := 0
	if opts.FloorApplied {
		floorApplied = 1
	}

	result, err := s.db.ExecContext(ctx, `
		INSERT INTO routing_decisions (
			dispatch_id, run_id, session_id, bead_id,
			project_dir, phase, agent, category,
			selected_model, rule_matched,
			floor_applied, floor_from, floor_to,
			candidates, excluded,
			policy_hash, override_id, complexity, context_json,
			decided_at
		) VALUES (
			NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''),
			?, NULLIF(?, ''), ?, NULLIF(?, ''),
			?, ?,
			?, NULLIF(?, ''), NULLIF(?, ''),
			NULLIF(?, ''), NULLIF(?, ''),
			NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, 0), NULLIF(?, ''),
			?)`,
		opts.DispatchID, opts.RunID, opts.SessionID, opts.BeadID,
		opts.ProjectDir, opts.Phase, opts.Agent, opts.Category,
		opts.SelectedModel, opts.RuleMatched,
		floorApplied, opts.FloorFrom, opts.FloorTo,
		opts.Candidates, opts.Excluded,
		opts.PolicyHash, opts.OverrideID, opts.Complexity, opts.ContextJSON,
		now,
	)
	if err != nil {
		return 0, fmt.Errorf("record routing decision: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("record routing decision: last id: %w", err)
	}
	return id, nil
}

// Get retrieves a routing decision by ID.
func (s *DecisionStore) Get(ctx context.Context, id int64) (*Decision, error) {
	d := &Decision{}
	var (
		dispatchID sql.NullString
		runID      sql.NullString
		sessionID  sql.NullString
		beadID     sql.NullString
		phase      sql.NullString
		category   sql.NullString
		floorFrom  sql.NullString
		floorTo    sql.NullString
		candidates sql.NullString
		excluded   sql.NullString
		policyHash sql.NullString
		overrideID sql.NullString
		complexity sql.NullInt64
		contextJSON sql.NullString
		floorApplied int
	)

	err := s.db.QueryRowContext(ctx, `
		SELECT id, dispatch_id, run_id, session_id, bead_id,
			project_dir, phase, agent, category,
			selected_model, rule_matched,
			floor_applied, floor_from, floor_to,
			candidates, excluded,
			policy_hash, override_id, complexity, context_json,
			decided_at
		FROM routing_decisions WHERE id = ?`, id).Scan(
		&d.ID, &dispatchID, &runID, &sessionID, &beadID,
		&d.ProjectDir, &phase, &d.Agent, &category,
		&d.SelectedModel, &d.RuleMatched,
		&floorApplied, &floorFrom, &floorTo,
		&candidates, &excluded,
		&policyHash, &overrideID, &complexity, &contextJSON,
		&d.DecidedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("routing decision not found: %d", id)
		}
		return nil, fmt.Errorf("get routing decision: %w", err)
	}

	d.DispatchID = decNullStr(dispatchID)
	d.RunID = decNullStr(runID)
	d.SessionID = decNullStr(sessionID)
	d.BeadID = decNullStr(beadID)
	d.Phase = decNullStr(phase)
	d.Category = decNullStr(category)
	d.FloorApplied = floorApplied != 0
	d.FloorFrom = decNullStr(floorFrom)
	d.FloorTo = decNullStr(floorTo)
	d.Candidates = decNullStr(candidates)
	d.Excluded = decNullStr(excluded)
	d.PolicyHash = decNullStr(policyHash)
	d.OverrideID = decNullStr(overrideID)
	d.Complexity = decNullIntPtr(complexity)
	d.ContextJSON = decNullStr(contextJSON)

	return d, nil
}

// List returns routing decisions matching the given filters.
func (s *DecisionStore) List(ctx context.Context, opts ListDecisionOpts) ([]Decision, error) {
	var where []string
	var args []interface{}

	if opts.ProjectDir != "" {
		where = append(where, "project_dir = ?")
		args = append(args, opts.ProjectDir)
	}
	if opts.Agent != "" {
		where = append(where, "agent = ?")
		args = append(args, opts.Agent)
	}
	if opts.Model != "" {
		where = append(where, "selected_model = ?")
		args = append(args, opts.Model)
	}
	if opts.DispatchID != "" {
		where = append(where, "dispatch_id = ?")
		args = append(args, opts.DispatchID)
	}
	if opts.Since > 0 {
		where = append(where, "decided_at >= ?")
		args = append(args, opts.Since)
	}
	if opts.Until > 0 {
		where = append(where, "decided_at <= ?")
		args = append(args, opts.Until)
	}

	query := `SELECT id, dispatch_id, run_id, session_id, bead_id,
		project_dir, phase, agent, category,
		selected_model, rule_matched,
		floor_applied, floor_from, floor_to,
		candidates, excluded,
		policy_hash, override_id, complexity, context_json,
		decided_at
		FROM routing_decisions`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY decided_at DESC"

	limit := opts.Limit
	if limit <= 0 {
		limit = 1000
	}
	query += fmt.Sprintf(" LIMIT %d", limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list routing decisions: %w", err)
	}
	defer rows.Close()

	var decisions []Decision
	for rows.Next() {
		d := Decision{}
		var (
			dispatchID  sql.NullString
			runID       sql.NullString
			sessionID   sql.NullString
			beadID      sql.NullString
			phase       sql.NullString
			category    sql.NullString
			floorFrom   sql.NullString
			floorTo     sql.NullString
			candidates  sql.NullString
			excluded    sql.NullString
			policyHash  sql.NullString
			overrideID  sql.NullString
			complexity  sql.NullInt64
			contextJSON sql.NullString
			floorApplied int
		)

		if err := rows.Scan(
			&d.ID, &dispatchID, &runID, &sessionID, &beadID,
			&d.ProjectDir, &phase, &d.Agent, &category,
			&d.SelectedModel, &d.RuleMatched,
			&floorApplied, &floorFrom, &floorTo,
			&candidates, &excluded,
			&policyHash, &overrideID, &complexity, &contextJSON,
			&d.DecidedAt,
		); err != nil {
			return nil, fmt.Errorf("list routing decisions: scan: %w", err)
		}

		d.DispatchID = decNullStr(dispatchID)
		d.RunID = decNullStr(runID)
		d.SessionID = decNullStr(sessionID)
		d.BeadID = decNullStr(beadID)
		d.Phase = decNullStr(phase)
		d.Category = decNullStr(category)
		d.FloorApplied = floorApplied != 0
		d.FloorFrom = decNullStr(floorFrom)
		d.FloorTo = decNullStr(floorTo)
		d.Candidates = decNullStr(candidates)
		d.Excluded = decNullStr(excluded)
		d.PolicyHash = decNullStr(policyHash)
		d.OverrideID = decNullStr(overrideID)
		d.Complexity = decNullIntPtr(complexity)
		d.ContextJSON = decNullStr(contextJSON)

		decisions = append(decisions, d)
	}
	return decisions, rows.Err()
}

// decNullStr converts sql.NullString to *string.
// Named with "dec" prefix to avoid collision with the package-level nullStr in resolve.go.
func decNullStr(ns sql.NullString) *string {
	if !ns.Valid {
		return nil
	}
	return &ns.String
}

// decNullIntPtr converts sql.NullInt64 to *int.
func decNullIntPtr(ni sql.NullInt64) *int {
	if !ni.Valid {
		return nil
	}
	v := int(ni.Int64)
	return &v
}
