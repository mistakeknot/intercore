package phase

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
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

	// Determine initial phase from chain
	initialPhase := PhaseBrainstorm
	if len(r.Phases) > 0 {
		initialPhase = r.Phases[0]
	}

	// Marshal phases to JSON if set
	var phasesJSON *string
	if r.Phases != nil {
		b, err := json.Marshal(r.Phases)
		if err != nil {
			return "", fmt.Errorf("run create: marshal phases: %w", err)
		}
		s := string(b)
		phasesJSON = &s
	}

	budgetWarnPct := r.BudgetWarnPct
	if budgetWarnPct == 0 {
		budgetWarnPct = 80 // default
	}

	// Marshal gate rules to JSON if set
	var gateRulesJSON *string
	if r.GateRules != nil {
		b, err := json.Marshal(r.GateRules)
		if err != nil {
			return "", fmt.Errorf("run create: marshal gate_rules: %w", err)
		}
		s := string(b)
		gateRulesJSON = &s
	}

	now := time.Now().Unix()
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO runs (
			id, project_dir, goal, status, phase, complexity,
			force_full, auto_advance, created_at, updated_at,
			scope_id, metadata, phases, token_budget, budget_warn_pct,
			parent_run_id, max_dispatches, budget_enforce, max_agents, gate_rules
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, r.ProjectDir, r.Goal, StatusActive, initialPhase,
		r.Complexity, boolToInt(r.ForceFull), boolToInt(r.AutoAdvance),
		now, now, r.ScopeID, r.Metadata,
		phasesJSON, r.TokenBudget, budgetWarnPct,
		r.ParentRunID, r.MaxDispatches,
		boolToInt(r.BudgetEnforce), r.MaxAgents,
		gateRulesJSON,
	)
	if err != nil {
		return "", fmt.Errorf("run create: %w", err)
	}

	idPayload, merr := json.Marshal(map[string]string{"generated_id": id})
	if merr == nil {
		artifactRef := "run:" + id
		if err := insertReplayInput(
			ctx,
			s.db.ExecContext,
			id,
			"random",
			"run_id",
			string(idPayload),
			artifactRef,
			"run_create",
			nil,
		); err != nil {
			return "", fmt.Errorf("run create: replay input: %w", err)
		}
	}
	return id, nil
}

// Get retrieves a run by ID.
func (s *Store) Get(ctx context.Context, id string) (*Run, error) {
	r := &Run{}
	var (
		completedAt   sql.NullInt64
		scopeID       sql.NullString
		metadata      sql.NullString
		forceFull     int
		autoAdvance   int
		phasesJSON    sql.NullString
		tokenBudget   sql.NullInt64
		budgetWarnPct sql.NullInt64
		parentRunID   sql.NullString
		maxDispatches sql.NullInt64
		budgetEnforce sql.NullInt64
		maxAgents     sql.NullInt64
		gateRulesJSON sql.NullString
	)

	err := s.db.QueryRowContext(ctx, `
		SELECT `+runCols+`
		FROM runs WHERE id = ?`, id).Scan(
		&r.ID, &r.ProjectDir, &r.Goal, &r.Status, &r.Phase,
		&r.Complexity, &forceFull, &autoAdvance,
		&r.CreatedAt, &r.UpdatedAt,
		&completedAt, &scopeID, &metadata,
		&phasesJSON, &tokenBudget, &budgetWarnPct,
		&parentRunID, &maxDispatches,
		&budgetEnforce, &maxAgents,
		&gateRulesJSON,
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
	r.TokenBudget = nullInt64(tokenBudget)
	r.BudgetWarnPct = int(nullInt64OrDefault(budgetWarnPct, 80))
	r.ParentRunID = nullStr(parentRunID)
	r.MaxDispatches = int(nullInt64OrDefault(maxDispatches, 0))
	r.BudgetEnforce = nullInt64OrDefault(budgetEnforce, 0) != 0
	r.MaxAgents = int(nullInt64OrDefault(maxAgents, 0))
	phases, err := parsePhasesJSON(phasesJSON)
	if err != nil {
		return nil, fmt.Errorf("run get: %w", err)
	}
	r.Phases = phases
	gr, err := parseGateRulesJSON(gateRulesJSON)
	if err != nil {
		return nil, fmt.Errorf("run get: %w", err)
	}
	r.GateRules = gr

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

// RollbackPhase rewinds a run's phase pointer backward.
// Uses optimistic concurrency on the phase column (AND phase = ?) to prevent
// TOCTOU races with concurrent Advance calls.
// Rejects cancelled/failed runs but allows completed (reverts to active).
func (s *Store) RollbackPhase(ctx context.Context, id, currentPhase, targetPhase string) error {
	now := time.Now().Unix()
	result, err := s.db.ExecContext(ctx, `
		UPDATE runs SET phase = ?, status = 'active', updated_at = ?, completed_at = NULL
		WHERE id = ? AND phase = ? AND status NOT IN ('cancelled', 'failed')`,
		targetPhase, now, id, currentPhase,
	)
	if err != nil {
		return fmt.Errorf("rollback phase: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("rollback phase: %w", err)
	}
	if n == 0 {
		// Distinguish: not found vs stale phase vs terminal status
		run, getErr := s.Get(ctx, id)
		if getErr != nil {
			return ErrNotFound
		}
		if run.Status == StatusCancelled || run.Status == StatusFailed {
			return ErrTerminalRun
		}
		return ErrStalePhase
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
	envelopeJSON := e.EnvelopeJSON
	if envelopeJSON == nil {
		envelopeJSON = defaultPhaseEnvelopeJSON(e.RunID, e.EventType, e.FromPhase, e.ToPhase)
	}

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO phase_events (
			run_id, from_phase, to_phase, event_type,
			gate_result, gate_tier, reason, envelope_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		e.RunID, e.FromPhase, e.ToPhase, e.EventType,
		e.GateResult, e.GateTier, e.Reason, envelopeJSON,
	)
	if err != nil {
		return fmt.Errorf("event add: %w", err)
	}

	var eventIDPtr *int64
	artifactRef := ""
	if eventID, idErr := res.LastInsertId(); idErr == nil && eventID > 0 {
		eventIDPtr = &eventID
		artifactRef = fmt.Sprintf("phase_event:%d", eventID)
	}
	if err := insertReplayInput(
		ctx,
		s.db.ExecContext,
		e.RunID,
		"time",
		"phase_transition",
		phaseReplayPayload(e),
		artifactRef,
		"phase",
		eventIDPtr,
	); err != nil {
		return fmt.Errorf("event add replay input: %w", err)
	}
	return nil
}

// Events returns all phase events for a run, ordered by id ASC.
func (s *Store) Events(ctx context.Context, runID string) ([]*PhaseEvent, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, run_id, from_phase, to_phase, event_type,
			gate_result, gate_tier, reason, envelope_json, created_at
		FROM phase_events WHERE run_id = ? ORDER BY id ASC`, runID)
	if err != nil {
		return nil, fmt.Errorf("events list: %w", err)
	}
	defer rows.Close()

	var events []*PhaseEvent
	for rows.Next() {
		e := &PhaseEvent{}
		var (
			gateResult   sql.NullString
			gateTier     sql.NullString
			reason       sql.NullString
			envelopeJSON sql.NullString
		)
		if err := rows.Scan(
			&e.ID, &e.RunID, &e.FromPhase, &e.ToPhase, &e.EventType,
			&gateResult, &gateTier, &reason, &envelopeJSON, &e.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("events scan: %w", err)
		}
		e.GateResult = nullStr(gateResult)
		e.GateTier = nullStr(gateTier)
		e.Reason = nullStr(reason)
		e.EnvelopeJSON = nullStr(envelopeJSON)
		events = append(events, e)
	}
	return events, rows.Err()
}

// Current returns the most recent active run for a project directory.
// Multiple active runs per project are allowed; this returns the newest.
// Returns ErrNotFound if no active run exists.
func (s *Store) Current(ctx context.Context, projectDir string) (*Run, error) {
	if projectDir == "" {
		return nil, fmt.Errorf("run current: empty project_dir (use GetChildren for portfolio runs)")
	}
	r := &Run{}
	var (
		completedAt   sql.NullInt64
		scopeID       sql.NullString
		metadata      sql.NullString
		forceFull     int
		autoAdvance   int
		phasesJSON    sql.NullString
		tokenBudget   sql.NullInt64
		budgetWarnPct sql.NullInt64
		parentRunID   sql.NullString
		maxDispatches sql.NullInt64
		budgetEnforce sql.NullInt64
		maxAgents     sql.NullInt64
		gateRulesJSON sql.NullString
	)

	err := s.db.QueryRowContext(ctx, `
		SELECT `+runCols+` FROM runs
		WHERE status = 'active' AND project_dir = ?
		ORDER BY created_at DESC, rowid DESC LIMIT 1`, projectDir).Scan(
		&r.ID, &r.ProjectDir, &r.Goal, &r.Status, &r.Phase,
		&r.Complexity, &forceFull, &autoAdvance,
		&r.CreatedAt, &r.UpdatedAt,
		&completedAt, &scopeID, &metadata,
		&phasesJSON, &tokenBudget, &budgetWarnPct,
		&parentRunID, &maxDispatches,
		&budgetEnforce, &maxAgents,
		&gateRulesJSON,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("run current: %w", err)
	}

	r.ForceFull = forceFull != 0
	r.AutoAdvance = autoAdvance != 0
	r.CompletedAt = nullInt64(completedAt)
	r.ScopeID = nullStr(scopeID)
	r.Metadata = nullStr(metadata)
	r.TokenBudget = nullInt64(tokenBudget)
	r.BudgetWarnPct = int(nullInt64OrDefault(budgetWarnPct, 80))
	r.ParentRunID = nullStr(parentRunID)
	r.MaxDispatches = int(nullInt64OrDefault(maxDispatches, 0))
	r.BudgetEnforce = nullInt64OrDefault(budgetEnforce, 0) != 0
	r.MaxAgents = int(nullInt64OrDefault(maxAgents, 0))
	phases, err := parsePhasesJSON(phasesJSON)
	if err != nil {
		return nil, fmt.Errorf("run current: %w", err)
	}
	r.Phases = phases
	gr, err := parseGateRulesJSON(gateRulesJSON)
	if err != nil {
		return nil, fmt.Errorf("run current: %w", err)
	}
	r.GateRules = gr

	return r, nil
}

// GetChildren returns all child runs of a portfolio run.
func (s *Store) GetChildren(ctx context.Context, parentRunID string) ([]*Run, error) {
	return s.queryRuns(ctx,
		"SELECT "+runCols+" FROM runs WHERE parent_run_id = ? ORDER BY created_at ASC", parentRunID)
}

// IsPortfolio returns true if the run is a portfolio run (has empty project_dir).
func (s *Store) IsPortfolio(ctx context.Context, runID string) (bool, error) {
	var projectDir string
	err := s.db.QueryRowContext(ctx, "SELECT project_dir FROM runs WHERE id = ?", runID).Scan(&projectDir)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, ErrNotFound
		}
		return false, fmt.Errorf("is portfolio: %w", err)
	}
	return projectDir == "", nil
}

// CreatePortfolio creates a portfolio run with children in a single transaction.
// Returns (portfolioID, childIDs, error).
func (s *Store) CreatePortfolio(ctx context.Context, portfolio *Run, children []*Run) (string, []string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", nil, fmt.Errorf("create portfolio: begin: %w", err)
	}
	defer tx.Rollback()

	portfolioID, err := generateID()
	if err != nil {
		return "", nil, err
	}

	initialPhase := PhaseBrainstorm
	if len(portfolio.Phases) > 0 {
		initialPhase = portfolio.Phases[0]
	}
	var phasesJSON *string
	if portfolio.Phases != nil {
		b, err := json.Marshal(portfolio.Phases)
		if err != nil {
			return "", nil, fmt.Errorf("create portfolio: marshal phases: %w", err)
		}
		s := string(b)
		phasesJSON = &s
	}
	budgetWarnPct := portfolio.BudgetWarnPct
	if budgetWarnPct == 0 {
		budgetWarnPct = 80
	}
	now := time.Now().Unix()

	// Marshal gate rules to JSON if set
	var portfolioGateRulesJSON *string
	if portfolio.GateRules != nil {
		b, err := json.Marshal(portfolio.GateRules)
		if err != nil {
			return "", nil, fmt.Errorf("create portfolio: marshal gate_rules: %w", err)
		}
		s := string(b)
		portfolioGateRulesJSON = &s
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO runs (
			id, project_dir, goal, status, phase, complexity,
			force_full, auto_advance, created_at, updated_at,
			scope_id, metadata, phases, token_budget, budget_warn_pct,
			parent_run_id, max_dispatches, budget_enforce, max_agents, gate_rules
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		portfolioID, "", portfolio.Goal, StatusActive, initialPhase,
		portfolio.Complexity, boolToInt(portfolio.ForceFull), boolToInt(portfolio.AutoAdvance),
		now, now, portfolio.ScopeID, portfolio.Metadata,
		phasesJSON, portfolio.TokenBudget, budgetWarnPct,
		nil, portfolio.MaxDispatches,
		boolToInt(portfolio.BudgetEnforce), portfolio.MaxAgents,
		portfolioGateRulesJSON,
	)
	if err != nil {
		return "", nil, fmt.Errorf("create portfolio: insert portfolio: %w", err)
	}

	childIDs := make([]string, 0, len(children))
	for _, child := range children {
		childID, err := generateID()
		if err != nil {
			return "", nil, err
		}
		childInitPhase := PhaseBrainstorm
		if len(child.Phases) > 0 {
			childInitPhase = child.Phases[0]
		}
		var childPhasesJSON *string
		if child.Phases != nil {
			b, err := json.Marshal(child.Phases)
			if err != nil {
				return "", nil, fmt.Errorf("create portfolio: marshal child phases: %w", err)
			}
			s := string(b)
			childPhasesJSON = &s
		}
		childBudgetWarnPct := child.BudgetWarnPct
		if childBudgetWarnPct == 0 {
			childBudgetWarnPct = 80
		}

		_, err = tx.ExecContext(ctx, `
			INSERT INTO runs (
				id, project_dir, goal, status, phase, complexity,
				force_full, auto_advance, created_at, updated_at,
				scope_id, metadata, phases, token_budget, budget_warn_pct,
				parent_run_id, max_dispatches, budget_enforce, max_agents
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			childID, child.ProjectDir, child.Goal, StatusActive, childInitPhase,
			child.Complexity, boolToInt(child.ForceFull), boolToInt(child.AutoAdvance),
			now, now, child.ScopeID, child.Metadata,
			childPhasesJSON, child.TokenBudget, childBudgetWarnPct,
			portfolioID, 0,
			boolToInt(child.BudgetEnforce), child.MaxAgents,
		)
		if err != nil {
			return "", nil, fmt.Errorf("create portfolio: insert child %s: %w", child.ProjectDir, err)
		}
		childIDs = append(childIDs, childID)
	}

	if err := tx.Commit(); err != nil {
		return "", nil, fmt.Errorf("create portfolio: commit: %w", err)
	}
	return portfolioID, childIDs, nil
}

// CancelPortfolio cancels a portfolio run and all its active children in a single transaction.
func (s *Store) CancelPortfolio(ctx context.Context, portfolioRunID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("cancel portfolio: begin: %w", err)
	}
	defer tx.Rollback()

	now := time.Now().Unix()

	// Cancel portfolio run itself
	result, err := tx.ExecContext(ctx, `
		UPDATE runs SET status = ?, updated_at = ?, completed_at = ?
		WHERE id = ? AND status = ?`,
		StatusCancelled, now, now, portfolioRunID, StatusActive,
	)
	if err != nil {
		return fmt.Errorf("cancel portfolio: update portfolio: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("cancel portfolio: rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("cancel portfolio: run %s not found or not active", portfolioRunID)
	}

	// Cancel all active children
	_, err = tx.ExecContext(ctx, `
		UPDATE runs SET status = ?, updated_at = ?, completed_at = ?
		WHERE parent_run_id = ? AND status = ?`,
		StatusCancelled, now, now, portfolioRunID, StatusActive,
	)
	if err != nil {
		return fmt.Errorf("cancel portfolio: update children: %w", err)
	}

	return tx.Commit()
}

// UpdateMaxDispatches sets the max_dispatches field for a portfolio run.
func (s *Store) UpdateMaxDispatches(ctx context.Context, id string, maxDispatches int) error {
	now := time.Now().Unix()
	result, err := s.db.ExecContext(ctx, `
		UPDATE runs SET max_dispatches = ?, updated_at = ? WHERE id = ?`,
		maxDispatches, now, id,
	)
	if err != nil {
		return fmt.Errorf("update max dispatches: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("update max dispatches: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SkipPhase marks a future phase as skipped with an audit trail.
// The phase must exist in the run's chain and must be ahead of the current phase.
// It does NOT advance the run — the next Advance will walk past it.
func (s *Store) SkipPhase(ctx context.Context, runID, targetPhase, reason, actor string) error {
	run, err := s.Get(ctx, runID)
	if err != nil {
		return err
	}
	if IsTerminalStatus(run.Status) {
		return ErrTerminalRun
	}

	chain := ResolveChain(run)

	// Validate target phase exists in chain
	if !ChainContains(chain, targetPhase) {
		return fmt.Errorf("skip: phase %q not in chain", targetPhase)
	}

	// Validate target is ahead of current (can't skip current or past phases)
	if !ChainIsValidTransition(chain, run.Phase, targetPhase) {
		return fmt.Errorf("skip: phase %q is not ahead of current phase %q", targetPhase, run.Phase)
	}

	// Guard: can't skip the terminal phase
	if ChainIsTerminal(chain, targetPhase) {
		return fmt.Errorf("skip: cannot skip terminal phase %q", targetPhase)
	}

	// Build reason string
	reasonStr := reason
	if actor != "" {
		reasonStr = fmt.Sprintf("actor=%s: %s", actor, reason)
	}

	return s.AddEvent(ctx, &PhaseEvent{
		RunID:     runID,
		FromPhase: run.Phase,
		ToPhase:   targetPhase,
		EventType: EventSkip,
		Reason:    strPtrOrNil(reasonStr),
	})
}

// SkippedPhases returns the set of phases that have been pre-skipped for a run.
func (s *Store) SkippedPhases(ctx context.Context, runID string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT to_phase FROM phase_events
		WHERE run_id = ? AND event_type = 'skip'`, runID)
	if err != nil {
		return nil, fmt.Errorf("skipped phases: %w", err)
	}
	defer rows.Close()

	skipped := make(map[string]bool)
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("skipped phases scan: %w", err)
		}
		skipped[p] = true
	}
	return skipped, rows.Err()
}

// --- helpers ---

const runCols = `id, project_dir, goal, status, phase, complexity,
	force_full, auto_advance, created_at, updated_at,
	completed_at, scope_id, metadata, phases, token_budget, budget_warn_pct,
	parent_run_id, max_dispatches, budget_enforce, max_agents, gate_rules`

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
			completedAt   sql.NullInt64
			scopeID       sql.NullString
			metadata      sql.NullString
			forceFull     int
			autoAdvance   int
			phasesJSON    sql.NullString
			tokenBudget   sql.NullInt64
			budgetWarnPct sql.NullInt64
			parentRunID   sql.NullString
			maxDispatches sql.NullInt64
			budgetEnforce sql.NullInt64
			maxAgents     sql.NullInt64
			gateRulesJSON sql.NullString
		)
		if err := rows.Scan(
			&r.ID, &r.ProjectDir, &r.Goal, &r.Status, &r.Phase,
			&r.Complexity, &forceFull, &autoAdvance,
			&r.CreatedAt, &r.UpdatedAt,
			&completedAt, &scopeID, &metadata,
			&phasesJSON, &tokenBudget, &budgetWarnPct,
			&parentRunID, &maxDispatches,
			&budgetEnforce, &maxAgents,
			&gateRulesJSON,
		); err != nil {
			return nil, fmt.Errorf("run list scan: %w", err)
		}
		r.ForceFull = forceFull != 0
		r.AutoAdvance = autoAdvance != 0
		r.CompletedAt = nullInt64(completedAt)
		r.ScopeID = nullStr(scopeID)
		r.Metadata = nullStr(metadata)
		r.TokenBudget = nullInt64(tokenBudget)
		r.BudgetWarnPct = int(nullInt64OrDefault(budgetWarnPct, 80))
		r.ParentRunID = nullStr(parentRunID)
		r.MaxDispatches = int(nullInt64OrDefault(maxDispatches, 0))
		r.BudgetEnforce = nullInt64OrDefault(budgetEnforce, 0) != 0
		r.MaxAgents = int(nullInt64OrDefault(maxAgents, 0))
		phases, err := parsePhasesJSON(phasesJSON)
		if err != nil {
			return nil, fmt.Errorf("run list: %w", err)
		}
		r.Phases = phases
		gr, err := parseGateRulesJSON(gateRulesJSON)
		if err != nil {
			return nil, fmt.Errorf("run list: %w", err)
		}
		r.GateRules = gr
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

// parsePhasesJSON decodes a nullable JSON phases column into a string slice.
// Returns nil, nil if the column is NULL. Returns error if the column is non-NULL
// but contains invalid JSON (prevents silent fallback to DefaultPhaseChain).
func parsePhasesJSON(ns sql.NullString) ([]string, error) {
	if !ns.Valid {
		return nil, nil
	}
	var chain []string
	if err := json.Unmarshal([]byte(ns.String), &chain); err != nil {
		return nil, fmt.Errorf("decode phases JSON: %w", err)
	}
	return chain, nil
}

// parseGateRulesJSON decodes a nullable JSON gate_rules column.
// Returns nil, nil if the column is NULL.
func parseGateRulesJSON(ns sql.NullString) (map[string][]SpecGateRule, error) {
	if !ns.Valid {
		return nil, nil
	}
	var rules map[string][]SpecGateRule
	if err := json.Unmarshal([]byte(ns.String), &rules); err != nil {
		return nil, fmt.Errorf("decode gate_rules JSON: %w", err)
	}
	return rules, nil
}

// ParseGateRules parses and validates a JSON gate rules string.
// Keys are "from→to" transition pairs, values are arrays of gate checks.
// Valid check types: artifact_exists, agents_complete, verdict_exists, budget_not_exceeded.
// Valid tiers: "hard", "soft", or empty (inherit from priority).
func ParseGateRules(jsonStr string) (map[string][]SpecGateRule, error) {
	var rules map[string][]SpecGateRule
	if err := json.Unmarshal([]byte(jsonStr), &rules); err != nil {
		return nil, fmt.Errorf("parse gate rules: %w", err)
	}

	validChecks := map[string]bool{
		CheckArtifactExists:    true,
		CheckAgentsComplete:    true,
		CheckVerdictExists:     true,
		CheckBudgetNotExceeded: true,
	}
	validTiers := map[string]bool{"hard": true, "soft": true, "": true}

	for key, ruleList := range rules {
		if key == "" {
			return nil, fmt.Errorf("parse gate rules: empty transition key")
		}
		if _, _, ok := parseTransitionKey(key); !ok {
			return nil, fmt.Errorf("parse gate rules: invalid transition key %q (want from→to)", key)
		}
		for i, r := range ruleList {
			if !validChecks[r.Check] {
				return nil, fmt.Errorf("parse gate rules: %s[%d]: unknown check %q", key, i, r.Check)
			}
			if !validTiers[r.Tier] {
				return nil, fmt.Errorf("parse gate rules: %s[%d]: invalid tier %q (want hard, soft, or empty)", key, i, r.Tier)
			}
		}
	}

	return rules, nil
}

// ValidateGateRulesForChain ensures custom gate rules are valid for the active phase chain.
// Every adjacent transition in the chain must be present in rules.
// Use an explicit empty rule list ([]) to declare an ungated transition.
func ValidateGateRulesForChain(chain []string, rules map[string][]SpecGateRule) error {
	if rules == nil {
		return nil
	}
	if len(chain) < 2 {
		return fmt.Errorf("validate gate rules: active phase chain must contain at least 2 phases")
	}

	adjacent := make(map[string]struct{}, len(chain)-1)
	for i := 0; i < len(chain)-1; i++ {
		adjacent[chain[i]+"→"+chain[i+1]] = struct{}{}
	}

	for key := range rules {
		from, to, ok := parseTransitionKey(key)
		if !ok {
			return fmt.Errorf("validate gate rules: invalid transition key %q (want from→to)", key)
		}
		if _, ok := adjacent[key]; ok {
			continue
		}
		if ChainContains(chain, from) && ChainContains(chain, to) {
			return fmt.Errorf("validate gate rules: transition %q is not adjacent in active phase chain", key)
		}
		return fmt.Errorf("validate gate rules: transition %q not present in active phase chain", key)
	}

	missing := make([]string, 0, len(adjacent))
	for key := range adjacent {
		if _, ok := rules[key]; !ok {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("validate gate rules: missing transitions for active phase chain: %s (use explicit [] to mark ungated)", strings.Join(missing, ", "))
	}

	return nil
}

func parseTransitionKey(key string) (string, string, bool) {
	if strings.Count(key, "→") != 1 {
		return "", "", false
	}
	parts := strings.SplitN(key, "→", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func nullInt64OrDefault(ni sql.NullInt64, def int64) int64 {
	if ni.Valid {
		return ni.Int64
	}
	return def
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// GetGateSignals extracts classified gate signals (TP/FP/TN/FN) from phase_events.
// Returns signals newer than sinceID, plus the max event ID seen (cursor for pagination).
//
// Algorithm (three-pass + reclassification):
//  1. Pass 1 — scan gated events: classify block→TP, override→FP, advance+pass→TN, advance+fail→FP
//  2. Pass 2 — rollback cross-check: reclassify TNs as FN when a rollback covers their transition
//  3. Pass 3 — block→override dedup: remove TP when same run+transition was later overridden
func (s *Store) GetGateSignals(ctx context.Context, sinceID int64) ([]GateSignal, int64, error) {
	// --- Pass 1: scan gated events ---
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, run_id, from_phase, to_phase, event_type,
			gate_result, reason, created_at
		FROM phase_events
		WHERE id > ? AND gate_result IS NOT NULL AND gate_result != 'none'
		ORDER BY id ASC`, sinceID)
	if err != nil {
		return nil, sinceID, fmt.Errorf("get gate signals: pass 1: %w", err)
	}
	defer rows.Close()

	type signalEntry struct {
		signal GateSignal
		idx    int // position in signals slice
	}

	var signals []GateSignal
	var cursor int64 = sinceID

	// Track blocks by run+transition for Pass 3 dedup
	// key: "runID:from→to"
	blockIdx := make(map[string][]int) // indices into signals

	// Track TNs by run for Pass 2 rollback reclassification
	// key: "runID"
	tnByRun := make(map[string][]int) // indices into signals

	for rows.Next() {
		var (
			id         int64
			runID      string
			fromPhase  string
			toPhase    string
			eventType  string
			gateResult sql.NullString
			reason     sql.NullString
			createdAt  int64
		)
		if err := rows.Scan(&id, &runID, &fromPhase, &toPhase, &eventType,
			&gateResult, &reason, &createdAt); err != nil {
			return nil, sinceID, fmt.Errorf("get gate signals: pass 1 scan: %w", err)
		}
		if id > cursor {
			cursor = id
		}

		// Extract check types from reason JSON (GateEvidence.Conditions[].Check)
		checkTypes := extractCheckTypes(reason)
		transKey := runID + ":" + fromPhase + "→" + toPhase

		switch eventType {
		case EventBlock:
			// Candidate TP — may be reclassified in Pass 3 if overridden
			for _, ct := range checkTypes {
				idx := len(signals)
				signals = append(signals, GateSignal{
					EventID:    id,
					RunID:      runID,
					CheckType:  ct,
					FromPhase:  fromPhase,
					ToPhase:    toPhase,
					SignalType: "tp",
					CreatedAt:  createdAt,
				})
				blockIdx[transKey] = append(blockIdx[transKey], idx)
			}

		case EventOverride:
			// FP — the gate was wrong to block (human overrode it)
			category := extractOverrideCategory(reason)
			for _, ct := range checkTypes {
				signals = append(signals, GateSignal{
					EventID:    id,
					RunID:      runID,
					CheckType:  ct,
					FromPhase:  fromPhase,
					ToPhase:    toPhase,
					SignalType: "fp",
					CreatedAt:  createdAt,
					Category:   category,
				})
			}

		case EventAdvance:
			gr := ""
			if gateResult.Valid {
				gr = gateResult.String
			}
			if gr == GatePass {
				// Candidate TN — may be reclassified in Pass 2 if rolled back
				for _, ct := range checkTypes {
					idx := len(signals)
					signals = append(signals, GateSignal{
						EventID:    id,
						RunID:      runID,
						CheckType:  ct,
						FromPhase:  fromPhase,
						ToPhase:    toPhase,
						SignalType: "tn",
						CreatedAt:  createdAt,
					})
					tnByRun[runID] = append(tnByRun[runID], idx)
				}
			} else if gr == GateFail {
				// Soft gate override-by-advance — FP
				for _, ct := range checkTypes {
					signals = append(signals, GateSignal{
						EventID:    id,
						RunID:      runID,
						CheckType:  ct,
						FromPhase:  fromPhase,
						ToPhase:    toPhase,
						SignalType: "fp",
						CreatedAt:  createdAt,
					})
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, sinceID, fmt.Errorf("get gate signals: pass 1 rows: %w", err)
	}

	// --- Pass 2: rollback cross-check ---
	// Reclassify TNs as FN when a rollback covers their from→to transition.
	if len(tnByRun) > 0 {
		rbRows, err := s.db.QueryContext(ctx, `
			SELECT run_id, from_phase, to_phase
			FROM phase_events
			WHERE event_type = 'rollback' AND id > ?
			ORDER BY id ASC`, sinceID)
		if err != nil {
			return nil, sinceID, fmt.Errorf("get gate signals: pass 2: %w", err)
		}
		defer rbRows.Close()

		for rbRows.Next() {
			var rbRunID, rbFrom, rbTo string
			if err := rbRows.Scan(&rbRunID, &rbFrom, &rbTo); err != nil {
				return nil, sinceID, fmt.Errorf("get gate signals: pass 2 scan: %w", err)
			}

			indices, ok := tnByRun[rbRunID]
			if !ok {
				continue
			}

			// Rollback goes from rbFrom (current) back to rbTo (target).
			// Any TN advance whose toPhase is within the rolled-back span
			// (strictly after rbTo, up to and including rbFrom) is reclassified as FN.
			for _, idx := range indices {
				sig := &signals[idx]
				if sig.SignalType != "tn" {
					continue // already reclassified
				}
				if isInRolledBackSpan(sig.ToPhase, rbTo, rbFrom) {
					sig.SignalType = "fn"
				}
			}
		}
		if err := rbRows.Err(); err != nil {
			return nil, sinceID, fmt.Errorf("get gate signals: pass 2 rows: %w", err)
		}
	}

	// --- Pass 3: block→override dedup ---
	// For each override (FP), remove the preceding block (TP) for the same
	// run+transition. The override FP already accounts for this event pair.
	for transKey := range blockIdx {
		// Check if any override (FP) exists for this run+transition
		hasOverride := false
		for _, sig := range signals {
			if sig.SignalType == "fp" && sig.Category != "" {
				overKey := sig.RunID + ":" + sig.FromPhase + "→" + sig.ToPhase
				if overKey == transKey {
					hasOverride = true
					break
				}
			}
		}
		if !hasOverride {
			continue
		}
		// Remove the TP block signals for this transition (mark as empty)
		for _, idx := range blockIdx[transKey] {
			signals[idx].SignalType = "" // mark for removal
		}
	}

	// Compact: remove marked-for-removal entries
	result := make([]GateSignal, 0, len(signals))
	for _, sig := range signals {
		if sig.SignalType != "" {
			result = append(result, sig)
		}
	}

	return result, cursor, nil
}

// extractCheckTypes parses the reason JSON to extract check types from
// GateEvidence.Conditions[].Check. Returns a single-element slice with
// an empty check type if parsing fails (attributing to the transition as a whole).
func extractCheckTypes(reason sql.NullString) []string {
	if !reason.Valid || reason.String == "" {
		return []string{""}
	}

	var evidence GateEvidence
	if err := json.Unmarshal([]byte(reason.String), &evidence); err != nil {
		return []string{""}
	}

	if len(evidence.Conditions) == 0 {
		return []string{""}
	}

	// Collect unique failing check types (for blocks/fails) or all checks (for passes)
	seen := make(map[string]bool)
	var checks []string
	for _, c := range evidence.Conditions {
		if c.Check != "" && !seen[c.Check] {
			seen[c.Check] = true
			checks = append(checks, c.Check)
		}
	}
	if len(checks) == 0 {
		return []string{""}
	}
	return checks
}

// extractOverrideCategory parses override_category from the reason JSON.
// Returns "uncategorized" if parsing fails or field is absent (pre-F5 overrides).
func extractOverrideCategory(reason sql.NullString) string {
	if !reason.Valid || reason.String == "" {
		return "uncategorized"
	}

	var parsed struct {
		Category string `json:"override_category"`
	}
	if err := json.Unmarshal([]byte(reason.String), &parsed); err != nil || parsed.Category == "" {
		return "uncategorized"
	}
	return parsed.Category
}

// isInRolledBackSpan checks if phase is in the span (rbTarget, rbFrom] using
// the default phase chain. A rollback from rbFrom to rbTarget rolls back all
// phases strictly after rbTarget up to and including rbFrom.
func isInRolledBackSpan(phase, rbTarget, rbFrom string) bool {
	targetIdx := ChainPhaseIndex(DefaultPhaseChain, rbTarget)
	fromIdx := ChainPhaseIndex(DefaultPhaseChain, rbFrom)
	phaseIdx := ChainPhaseIndex(DefaultPhaseChain, phase)

	if targetIdx < 0 || fromIdx < 0 || phaseIdx < 0 {
		return false
	}
	// Phase is in rolled-back span if it's strictly after target and at or before from
	return phaseIdx > targetIdx && phaseIdx <= fromIdx
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
