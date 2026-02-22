package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// SpawnPolicy configures pre-spawn enforcement checks.
// Zero values mean "unlimited" (no constraint).
type SpawnPolicy struct {
	BudgetEnforce   bool // reject spawn if run budget exceeded
	MaxActivePerRun int  // max concurrent active dispatches per run
	MaxActiveGlobal int  // max concurrent active dispatches across all runs
	MaxAgentsPerRun int  // max total dispatches ever spawned for this run
	MaxSpawnDepth   int  // max parent→child dispatch nesting depth
}

// SpawnRejection describes why a spawn was rejected.
type SpawnRejection struct {
	Reason  string `json:"reason"`
	RunID   string `json:"run_id,omitempty"`
	Current int64  `json:"current"`
	Limit   int64  `json:"limit"`
}

func (r *SpawnRejection) Error() string {
	return fmt.Sprintf("spawn rejected: %s (current=%d, limit=%d)", r.Reason, r.Current, r.Limit)
}

// JSON returns the rejection as a JSON string.
func (r *SpawnRejection) JSON() string {
	b, _ := json.Marshal(r)
	return string(b)
}

// BudgetQuerier checks whether a run's budget is exceeded.
type BudgetQuerier interface {
	IsBudgetExceeded(ctx context.Context, runID string) (bool, error)
}

// CheckPolicy evaluates all spawn policy constraints.
// Returns nil if the spawn is allowed, or a *SpawnRejection if rejected.
func CheckPolicy(ctx context.Context, store *Store, budgetQ BudgetQuerier, policy SpawnPolicy, d *Dispatch) error {
	scopeID := ""
	if d.ScopeID != nil {
		scopeID = *d.ScopeID
	}

	// Budget enforcement
	if policy.BudgetEnforce && scopeID != "" && budgetQ != nil {
		exceeded, err := budgetQ.IsBudgetExceeded(ctx, scopeID)
		if err != nil {
			return fmt.Errorf("policy check budget: %w", err)
		}
		if exceeded {
			return &SpawnRejection{
				Reason: "budget_exceeded",
				RunID:  scopeID,
			}
		}
	}

	// Per-run concurrency limit
	if policy.MaxActivePerRun > 0 && scopeID != "" {
		count, err := store.CountActiveByScope(ctx, scopeID)
		if err != nil {
			return fmt.Errorf("policy check per-run concurrency: %w", err)
		}
		if count >= policy.MaxActivePerRun {
			return &SpawnRejection{
				Reason:  "concurrency_limit_per_run",
				RunID:   scopeID,
				Current: int64(count),
				Limit:   int64(policy.MaxActivePerRun),
			}
		}
	}

	// Global concurrency limit
	if policy.MaxActiveGlobal > 0 {
		count, err := store.CountActiveGlobal(ctx)
		if err != nil {
			return fmt.Errorf("policy check global concurrency: %w", err)
		}
		if count >= policy.MaxActiveGlobal {
			return &SpawnRejection{
				Reason:  "concurrency_limit_global",
				Current: int64(count),
				Limit:   int64(policy.MaxActiveGlobal),
			}
		}
	}

	// Agent cap (total ever spawned per run)
	if policy.MaxAgentsPerRun > 0 && scopeID != "" {
		count, err := store.CountTotalByScope(ctx, scopeID)
		if err != nil {
			return fmt.Errorf("policy check agent cap: %w", err)
		}
		if count >= policy.MaxAgentsPerRun {
			return &SpawnRejection{
				Reason:  "agent_cap_per_run",
				RunID:   scopeID,
				Current: int64(count),
				Limit:   int64(policy.MaxAgentsPerRun),
			}
		}
	}

	// Spawn depth limit
	if policy.MaxSpawnDepth > 0 && d.SpawnDepth > policy.MaxSpawnDepth {
		return &SpawnRejection{
			Reason:  "spawn_depth_exceeded",
			Current: int64(d.SpawnDepth),
			Limit:   int64(policy.MaxSpawnDepth),
		}
	}

	return nil
}

// IsSpawnRejection returns true if the error is a spawn rejection.
func IsSpawnRejection(err error) bool {
	var r *SpawnRejection
	return errors.As(err, &r)
}
