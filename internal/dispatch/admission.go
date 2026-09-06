package dispatch

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// InputError identifies an invalid spawn configuration (CLI exit 3).
type InputError struct{ Message string }

func (e *InputError) Error() string { return "spawn: " + e.Message }

// Validate checks configuration without database access, including scheduled requests.
func (o SpawnOptions) Validate() error {
	if o.ProjectDir == "" {
		return &InputError{"project_dir is required"}
	}
	if o.PromptFile == "" {
		return &InputError{"prompt_file is required"}
	}
	if o.AgentType == "flere" && o.RunID == "" {
		return &InputError{"flere requires an explicit run_id"}
	}
	if o.RunID != "" && o.ScopeID != "" && o.RunID != o.ScopeID {
		return &InputError{"run_id and scope_id must match"}
	}
	if o.Policy != nil {
		p := o.Policy
		if p.MaxActivePerRun < 0 || p.MaxActiveGlobal < 0 || p.MaxAgentsPerRun < 0 || p.MaxSpawnDepth < 0 {
			return &InputError{"spawn limits must be nonnegative"}
		}
		if (p.MaxActivePerRun > 0 || p.MaxAgentsPerRun > 0) && o.ScopeID == "" && o.RunID == "" && o.ParentDispatchID == "" {
			return &InputError{"per-run limits require a scope, run, or scoped parent"}
		}
	}
	return nil
}

type admissionQuerier interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// admit reserves a dispatch slot before process creation. BEGIN IMMEDIATE is
// intentional: modernc SQLite ignores sql.TxOptions.Isolation. Every run-policy,
// parent, usage, and count read and the insert use this one owned connection.
// No lock is held while calling external checkers or starting the process.
func (s *Store) admit(ctx context.Context, d *Dispatch, opts SpawnOptions) (string, error) {
	policy := SpawnPolicy{}
	if opts.Policy != nil {
		policy = *opts.Policy
	}
	// For enforced budgets, a legacy external checker is an additional veto,
	// not an atomic authority. Supplying a checker does not enforce an advisory run.
	// In particular, a checker cannot replace a positive budget stored on a run.
	// Calling it outside the transaction also permits a checker using this DB's
	// single connection. Scope and persisted policy are rechecked below.
	if opts.BudgetQuerier != nil {
		scope := opts.ScopeID
		if scope == "" && d.ParentDispatchID != "" {
			parent, err := s.Get(ctx, d.ParentDispatchID)
			if err != nil && !errors.Is(err, ErrNotFound) {
				return "", err
			}
			if parent != nil && parent.ScopeID != nil {
				scope = *parent.ScopeID
			}
		}
		enforce := policy.BudgetEnforce
		if scope != "" {
			var storedEnforce int
			err := s.db.QueryRowContext(ctx, `SELECT COALESCE(budget_enforce, 0) FROM runs WHERE id = ?`, scope).Scan(&storedEnforce)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return "", fmt.Errorf("read external budget policy: %w", err)
			}
			enforce = enforce || storedEnforce != 0
		}
		if enforce {
			if scope == "" {
				return "", &SpawnRejection{Reason: "budget_unavailable"}
			}
			exceeded, err := opts.BudgetQuerier.IsBudgetExceeded(ctx, scope)
			if err != nil {
				return "", fmt.Errorf("external budget check: %w", err)
			}
			if exceeded {
				return "", &SpawnRejection{Reason: "budget_exceeded", RunID: scope}
			}
		}
	}

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return "", fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		return "", fmt.Errorf("begin admission: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			// The request may already be cancelled; never return a connection with
			// an open transaction to the pool if cleanup itself fails.
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := conn.ExecContext(cleanup, "ROLLBACK"); err != nil {
				_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			}
		}
	}()

	// Kernel policy is authoritative too. Read it inside the same transaction
	// as run policy and reservation, and never silently accept malformed limits.
	for _, limit := range []struct {
		key    string
		target *int
	}{
		{"kernel.global_max_dispatches", &policy.MaxActiveGlobal},
		{"kernel.max_spawn_depth", &policy.MaxSpawnDepth},
	} {
		var raw string
		err := conn.QueryRowContext(ctx, `SELECT payload FROM state WHERE key=? AND scope_id='global' AND (expires_at IS NULL OR expires_at > unixepoch())`, limit.key).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("read stored kernel policy: %w", err)
		}
		value, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil || value < 0 {
			return "", fmt.Errorf("invalid stored kernel policy %s", limit.key)
		}
		if value > 0 && (*limit.target == 0 || value < *limit.target) {
			*limit.target = value
		}
	}

	if d.ParentDispatchID != "" {
		var parentScope sql.NullString
		var depth int
		err := conn.QueryRowContext(ctx, `SELECT scope_id, spawn_depth FROM dispatches WHERE id = ?`, d.ParentDispatchID).Scan(&parentScope, &depth)
		if errors.Is(err, sql.ErrNoRows) {
			return "", &SpawnRejection{Reason: "parent_not_found"}
		}
		if err != nil {
			return "", fmt.Errorf("read parent: %w", err)
		}
		if parentScope.Valid && parentScope.String != "" {
			if d.ScopeID != nil && *d.ScopeID != parentScope.String {
				return "", &SpawnRejection{Reason: "parent_scope_mismatch", RunID: parentScope.String}
			}
			d.ScopeID = &parentScope.String
		}
		d.SpawnDepth = depth + 1
		if opts.retry {
			d.SpawnDepth = depth
		}
	}

	scope := ""
	if d.ScopeID != nil {
		scope = *d.ScopeID
	}
	if scope == "" && (policy.MaxActivePerRun > 0 || policy.MaxAgentsPerRun > 0) {
		return "", &InputError{"per-run limits require a scope, run, or scoped parent"}
	}
	var tokenBudget sql.NullInt64
	if scope != "" {
		var enforce, maxAgents int
		err := conn.QueryRowContext(ctx, `SELECT COALESCE(budget_enforce, 0), token_budget, COALESCE(max_agents, 0) FROM runs WHERE id = ?`, scope).Scan(&enforce, &tokenBudget, &maxAgents)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("read run policy: %w", err)
		}
		if errors.Is(err, sql.ErrNoRows) && opts.RunID != "" {
			return "", &SpawnRejection{Reason: "run_not_found", RunID: scope}
		}
		policy.BudgetEnforce = policy.BudgetEnforce || enforce != 0
		if maxAgents > 0 && (policy.MaxAgentsPerRun == 0 || maxAgents < policy.MaxAgentsPerRun) {
			policy.MaxAgentsPerRun = maxAgents
		}
	}
	if policy.BudgetEnforce {
		if !tokenBudget.Valid || tokenBudget.Int64 <= 0 {
			return "", &SpawnRejection{Reason: "budget_unavailable", RunID: scope}
		}
		var used int64
		err := conn.QueryRowContext(ctx, `SELECT COALESCE(SUM(input_tokens), 0) + COALESCE(SUM(output_tokens), 0) FROM dispatches WHERE scope_id = ?`, scope).Scan(&used)
		if err != nil {
			return "", fmt.Errorf("read budget usage: %w", err)
		}
		if used >= tokenBudget.Int64 {
			return "", &SpawnRejection{Reason: "budget_exceeded", RunID: scope, Current: used, Limit: tokenBudget.Int64}
		}
	}
	if err := checkPolicyCounts(ctx, conn, policy, d); err != nil {
		return "", err
	}
	id, err := createDispatch(ctx, conn, d)
	if err != nil {
		return "", err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return "", fmt.Errorf("commit admission: %w", err)
	}
	committed = true
	return id, nil
}
