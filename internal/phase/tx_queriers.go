package phase

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Querier is satisfied by both *sql.DB and *sql.Tx, allowing gate checks
// and phase updates to share a single transaction for atomicity.
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row
}

// txRuntrackQuerier runs runtrack queries on a transaction.
// Replicates the SQL from runtrack.Store but operates on the provided Querier.
type txRuntrackQuerier struct {
	q Querier
}

func (t *txRuntrackQuerier) CountArtifacts(ctx context.Context, runID, phase string) (int, error) {
	var count int
	err := t.q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM run_artifacts WHERE run_id = ? AND phase = ? AND status = 'active'`,
		runID, phase).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("tx count artifacts: %w", err)
	}
	return count, nil
}

func (t *txRuntrackQuerier) CountActiveAgents(ctx context.Context, runID string) (int, error) {
	var count int
	err := t.q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM run_agents WHERE run_id = ? AND status = 'active'`,
		runID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("tx count active agents: %w", err)
	}
	return count, nil
}

// txVerdictQuerier runs verdict queries on a transaction.
// Replicates the SQL from dispatch.Store.HasVerdict.
type txVerdictQuerier struct {
	q Querier
}

func (t *txVerdictQuerier) HasVerdict(ctx context.Context, scopeID string) (bool, error) {
	if scopeID == "" {
		return false, nil
	}
	var count int
	err := t.q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM dispatches
			WHERE scope_id = ? AND verdict_status IS NOT NULL AND verdict_status != 'reject'`,
		scopeID).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("tx has verdict: %w", err)
	}
	return count > 0, nil
}

// txPortfolioQuerier runs portfolio queries on a transaction.
// Replicates the SQL from phase.Store.GetChildren.
type txPortfolioQuerier struct {
	q Querier
}

func (t *txPortfolioQuerier) GetChildren(ctx context.Context, parentRunID string) ([]*Run, error) {
	rows, err := t.q.QueryContext(ctx,
		"SELECT "+runCols+" FROM runs WHERE parent_run_id = ? ORDER BY created_at ASC", parentRunID)
	if err != nil {
		return nil, fmt.Errorf("tx get children: %w", err)
	}
	defer rows.Close()

	return scanRuns(rows)
}

// txDepQuerier runs dependency queries on a transaction.
// Replicates the SQL from portfolio.DepStore.GetUpstream.
type txDepQuerier struct {
	q Querier
}

func (t *txDepQuerier) GetUpstream(ctx context.Context, portfolioRunID, downstream string) ([]string, error) {
	rows, err := t.q.QueryContext(ctx, `
		SELECT upstream_project FROM project_deps
		WHERE portfolio_run_id = ? AND downstream_project = ?
		ORDER BY upstream_project ASC`, portfolioRunID, downstream)
	if err != nil {
		return nil, fmt.Errorf("tx get upstream: %w", err)
	}
	defer rows.Close()

	var projects []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("tx get upstream scan: %w", err)
		}
		projects = append(projects, p)
	}
	return projects, rows.Err()
}

// scanRuns scans run rows into []*Run. Shared between store.queryRuns and txPortfolioQuerier.
func scanRuns(rows *sql.Rows) ([]*Run, error) {
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
			return nil, fmt.Errorf("scan run: %w", err)
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
			return nil, err
		}
		r.Phases = phases
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

// --- Store methods that accept a Querier (used by Advance's transaction) ---

// GetQ retrieves a run by ID using the provided querier.
func (s *Store) GetQ(ctx context.Context, q Querier, id string) (*Run, error) {
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

	err := q.QueryRowContext(ctx, `
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

// UpdatePhaseQ transitions a run's phase using the provided querier.
func (s *Store) UpdatePhaseQ(ctx context.Context, q Querier, id, expectedPhase, newPhase string) error {
	now := time.Now().Unix()
	result, err := q.ExecContext(ctx, `
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
		_, getErr := s.GetQ(ctx, q, id)
		if getErr != nil {
			return ErrNotFound
		}
		return ErrStalePhase
	}
	return nil
}

// UpdateStatusQ sets a run's status using the provided querier.
func (s *Store) UpdateStatusQ(ctx context.Context, q Querier, id, status string) error {
	now := time.Now().Unix()
	var completedAt *int64
	if IsTerminalStatus(status) {
		completedAt = &now
	}

	result, err := q.ExecContext(ctx, `
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

// AddEventQ inserts a phase event using the provided querier.
func (s *Store) AddEventQ(ctx context.Context, q Querier, e *PhaseEvent) error {
	envelopeJSON := e.EnvelopeJSON
	if envelopeJSON == nil {
		envelopeJSON = defaultPhaseEnvelopeJSON(e.RunID, e.EventType, e.FromPhase, e.ToPhase)
	}

	res, err := q.ExecContext(ctx, `
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
		q.ExecContext,
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

// SkippedPhasesQ returns pre-skipped phases using the provided querier.
func (s *Store) SkippedPhasesQ(ctx context.Context, q Querier, runID string) (map[string]bool, error) {
	rows, err := q.QueryContext(ctx, `
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

// BeginTx starts a new transaction on the store's database.
func (s *Store) BeginTx(ctx context.Context) (*sql.Tx, error) {
	return s.db.BeginTx(ctx, nil)
}
