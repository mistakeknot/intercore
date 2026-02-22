package phase

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
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

	now := time.Now().Unix()
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO runs (
			id, project_dir, goal, status, phase, complexity,
			force_full, auto_advance, created_at, updated_at,
			scope_id, metadata, phases, token_budget, budget_warn_pct,
			parent_run_id, max_dispatches, budget_enforce, max_agents
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, r.ProjectDir, r.Goal, StatusActive, initialPhase,
		r.Complexity, boolToInt(r.ForceFull), boolToInt(r.AutoAdvance),
		now, now, r.ScopeID, r.Metadata,
		phasesJSON, r.TokenBudget, budgetWarnPct,
		r.ParentRunID, r.MaxDispatches,
		boolToInt(r.BudgetEnforce), r.MaxAgents,
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
		completedAt    sql.NullInt64
		scopeID        sql.NullString
		metadata       sql.NullString
		forceFull      int
		autoAdvance    int
		phasesJSON     sql.NullString
		tokenBudget    sql.NullInt64
		budgetWarnPct  sql.NullInt64
		parentRunID    sql.NullString
		maxDispatches  sql.NullInt64
		budgetEnforce  sql.NullInt64
		maxAgents      sql.NullInt64
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

// Current returns the most recent active run for a project directory.
// Multiple active runs per project are allowed; this returns the newest.
// Returns ErrNotFound if no active run exists.
func (s *Store) Current(ctx context.Context, projectDir string) (*Run, error) {
	if projectDir == "" {
		return nil, fmt.Errorf("run current: empty project_dir (use GetChildren for portfolio runs)")
	}
	r := &Run{}
	var (
		completedAt    sql.NullInt64
		scopeID        sql.NullString
		metadata       sql.NullString
		forceFull      int
		autoAdvance    int
		phasesJSON     sql.NullString
		tokenBudget    sql.NullInt64
		budgetWarnPct  sql.NullInt64
		parentRunID    sql.NullString
		maxDispatches  sql.NullInt64
		budgetEnforce  sql.NullInt64
		maxAgents      sql.NullInt64
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

	_, err = tx.ExecContext(ctx, `
		INSERT INTO runs (
			id, project_dir, goal, status, phase, complexity,
			force_full, auto_advance, created_at, updated_at,
			scope_id, metadata, phases, token_budget, budget_warn_pct,
			parent_run_id, max_dispatches, budget_enforce, max_agents
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		portfolioID, "", portfolio.Goal, StatusActive, initialPhase,
		portfolio.Complexity, boolToInt(portfolio.ForceFull), boolToInt(portfolio.AutoAdvance),
		now, now, portfolio.ScopeID, portfolio.Metadata,
		phasesJSON, portfolio.TokenBudget, budgetWarnPct,
		nil, portfolio.MaxDispatches,
		boolToInt(portfolio.BudgetEnforce), portfolio.MaxAgents,
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
	parent_run_id, max_dispatches, budget_enforce, max_agents`

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
			completedAt    sql.NullInt64
			scopeID        sql.NullString
			metadata       sql.NullString
			forceFull      int
			autoAdvance    int
			phasesJSON     sql.NullString
			tokenBudget    sql.NullInt64
			budgetWarnPct  sql.NullInt64
			parentRunID    sql.NullString
			maxDispatches  sql.NullInt64
			budgetEnforce  sql.NullInt64
			maxAgents      sql.NullInt64
		)
		if err := rows.Scan(
			&r.ID, &r.ProjectDir, &r.Goal, &r.Status, &r.Phase,
			&r.Complexity, &forceFull, &autoAdvance,
			&r.CreatedAt, &r.UpdatedAt,
			&completedAt, &scopeID, &metadata,
			&phasesJSON, &tokenBudget, &budgetWarnPct,
			&parentRunID, &maxDispatches,
			&budgetEnforce, &maxAgents,
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
