package budget

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mistakeknot/interverse/infra/intercore/internal/dispatch"
	"github.com/mistakeknot/interverse/infra/intercore/internal/phase"
	"github.com/mistakeknot/interverse/infra/intercore/internal/state"
)

// Event type constants for budget threshold crossings.
const (
	EventBudgetWarning  = "budget.warning"
	EventBudgetExceeded = "budget.exceeded"
)

// Result describes what happened when a budget was checked.
type Result struct {
	RunID     string
	Budget    int64
	Used      int64
	WarnPct   int
	Warning   bool // true if warning threshold was crossed this check
	Exceeded  bool // true if budget was exceeded this check
}

// PhaseStoreQuerier is the subset of phase.Store needed for budget checks.
type PhaseStoreQuerier interface {
	Get(ctx context.Context, id string) (*phase.Run, error)
}

// EventRecorder is called when a budget threshold is crossed.
type EventRecorder func(ctx context.Context, runID, eventType, reason string) error

// Checker evaluates token budgets for runs.
type Checker struct {
	phaseStore    PhaseStoreQuerier
	dispatchStore *dispatch.Store
	stateStore    *state.Store
	recorder      EventRecorder
}

// New creates a budget checker. recorder may be nil if event recording is not needed.
func New(ps PhaseStoreQuerier, ds *dispatch.Store, ss *state.Store, recorder EventRecorder) *Checker {
	return &Checker{
		phaseStore:    ps,
		dispatchStore: ds,
		stateStore:    ss,
		recorder:      recorder,
	}
}

// Check evaluates the token budget for a run. Returns nil Result if no budget is set.
func (c *Checker) Check(ctx context.Context, runID string) (*Result, error) {
	run, err := c.phaseStore.Get(ctx, runID)
	if err != nil {
		return nil, nil // run not found or error — no budget to check
	}
	if run.TokenBudget == nil || *run.TokenBudget <= 0 {
		return nil, nil // no budget set
	}

	agg, err := c.dispatchStore.AggregateTokens(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("budget check: %w", err)
	}

	budget := *run.TokenBudget
	used := agg.TotalIn + agg.TotalOut
	warnThreshold := budget * int64(run.BudgetWarnPct) / 100

	result := &Result{
		RunID:   runID,
		Budget:  budget,
		Used:    used,
		WarnPct: run.BudgetWarnPct,
	}

	// Check warning threshold
	if used >= warnThreshold && warnThreshold > 0 {
		key := "budget.warning"
		if !c.flagExists(ctx, key, runID) {
			result.Warning = true
			c.setFlag(ctx, key, runID, used)
			c.emitEvent(ctx, runID, EventBudgetWarning,
				fmt.Sprintf("token usage %d reached %d%% of budget %d", used, run.BudgetWarnPct, budget))
		}
	}

	// Check exceeded threshold
	if used >= budget {
		key := "budget.exceeded"
		if !c.flagExists(ctx, key, runID) {
			result.Exceeded = true
			c.setFlag(ctx, key, runID, used)
			c.emitEvent(ctx, runID, EventBudgetExceeded,
				fmt.Sprintf("token usage %d exceeded budget %d", used, budget))
		}
	}

	return result, nil
}

func (c *Checker) flagExists(ctx context.Context, key, scopeID string) bool {
	_, err := c.stateStore.Get(ctx, key, scopeID)
	return err == nil
}

func (c *Checker) setFlag(ctx context.Context, key, scopeID string, used int64) {
	payload, _ := json.Marshal(map[string]int64{"used": used})
	// Ignore error — flag is best-effort dedup
	c.stateStore.Set(ctx, key, scopeID, payload, 0)
}

func (c *Checker) emitEvent(ctx context.Context, runID, eventType, reason string) {
	if c.recorder != nil {
		// Ignore error — event recording is fire-and-forget
		c.recorder(ctx, runID, eventType, reason)
	}
}
