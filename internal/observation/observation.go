package observation

import (
	"context"
	"fmt"
	"time"

	"github.com/mistakeknot/intercore/internal/dispatch"
	"github.com/mistakeknot/intercore/internal/event"
	"github.com/mistakeknot/intercore/internal/phase"
)

// Snapshot is a unified observation of system state at a point in time.
type Snapshot struct {
	Timestamp  time.Time       `json:"timestamp"`
	Runs       []RunSummary    `json:"runs"`
	Dispatches DispatchSummary `json:"dispatches"`
	Events     []event.Event   `json:"recent_events"`
	Queue      QueueSummary    `json:"queue"`
	Budget     *BudgetSummary  `json:"budget,omitempty"`
}

// RunSummary is a condensed view of a phase run.
type RunSummary struct {
	ID         string `json:"id"`
	Phase      string `json:"phase"`
	Status     string `json:"status"`
	ProjectDir string `json:"project_dir"`
	Goal       string `json:"goal"`
	CreatedAt  int64  `json:"created_at"`
}

// DispatchSummary aggregates dispatch information.
type DispatchSummary struct {
	Active int            `json:"active"`
	Total  int            `json:"total"`
	Agents []AgentSummary `json:"agents"`
}

// AgentSummary is a condensed view of a dispatched agent.
type AgentSummary struct {
	ID        string `json:"id"`
	AgentType string `json:"agent_type"`
	Status    string `json:"status"`
	Turns     int    `json:"turns"`
	InputTok  int    `json:"input_tokens"`
	OutputTok int    `json:"output_tokens"`
	ScopeID   string `json:"scope_id,omitempty"`
}

// QueueSummary aggregates scheduler queue counts.
type QueueSummary struct {
	Pending  int `json:"pending"`
	Running  int `json:"running"`
	Retrying int `json:"retrying"`
}

// BudgetSummary is a condensed view of token budget state for a run.
type BudgetSummary struct {
	RunID     string `json:"run_id"`
	Budget    int64  `json:"budget"`
	Used      int64  `json:"used"`
	Remaining int64  `json:"remaining"`
}

// CollectOptions controls what the Collector gathers.
type CollectOptions struct {
	RunID      string
	EventLimit int
}

// PhaseQuerier is the subset of phase.Store needed by the Collector.
type PhaseQuerier interface {
	Get(ctx context.Context, id string) (*phase.Run, error)
	ListActive(ctx context.Context) ([]*phase.Run, error)
}

// DispatchQuerier is the subset of dispatch.Store needed by the Collector.
type DispatchQuerier interface {
	ListActive(ctx context.Context) ([]*dispatch.Dispatch, error)
	AggregateTokens(ctx context.Context, scopeID string) (*dispatch.TokenAggregation, error)
}

// EventQuerier is the subset of event.Store needed by the Collector.
type EventQuerier interface {
	ListAllEvents(ctx context.Context, sincePhaseID, sinceDispatchID, sinceDiscoveryID, sinceCoordinationID, sinceReviewID int64, limit int) ([]event.Event, error)
	ListEvents(ctx context.Context, runID string, sincePhaseID, sinceDispatchID, sinceCoordinationID, sinceReviewID int64, limit int) ([]event.Event, error)
}

// SchedulerQuerier is the subset of scheduler.Store needed by the Collector.
type SchedulerQuerier interface {
	CountByStatus(ctx context.Context) (map[string]int, error)
}

// Collector aggregates system state from multiple stores into a Snapshot.
type Collector struct {
	phases     PhaseQuerier
	dispatches DispatchQuerier
	events     EventQuerier
	scheduler  SchedulerQuerier
}

// NewCollector creates a Collector. Any dependency may be nil; the Collector
// will skip queries for nil stores.
func NewCollector(p PhaseQuerier, d DispatchQuerier, e EventQuerier, s SchedulerQuerier) *Collector {
	return &Collector{
		phases:     p,
		dispatches: d,
		events:     e,
		scheduler:  s,
	}
}

// Collect gathers a snapshot of system state. If opts.RunID is set, the
// snapshot is scoped to that run (including budget info). Otherwise it
// returns all active runs and global events.
func (c *Collector) Collect(ctx context.Context, opts CollectOptions) (*Snapshot, error) {
	if opts.EventLimit <= 0 {
		opts.EventLimit = 20
	}

	snap := &Snapshot{
		Timestamp: time.Now().UTC(),
		Runs:      []RunSummary{},
		Events:    []event.Event{},
		Dispatches: DispatchSummary{
			Agents: []AgentSummary{},
		},
	}

	// Phases
	if c.phases != nil {
		if opts.RunID != "" {
			run, err := c.phases.Get(ctx, opts.RunID)
			if err != nil {
				return nil, fmt.Errorf("observation: get run %s: %w", opts.RunID, err)
			}
			snap.Runs = []RunSummary{runToSummary(run)}
		} else {
			runs, err := c.phases.ListActive(ctx)
			if err != nil {
				return nil, fmt.Errorf("observation: list active runs: %w", err)
			}
			for _, r := range runs {
				snap.Runs = append(snap.Runs, runToSummary(r))
			}
		}
	}

	// Dispatches
	if c.dispatches != nil {
		active, err := c.dispatches.ListActive(ctx)
		if err != nil {
			return nil, fmt.Errorf("observation: list active dispatches: %w", err)
		}
		for _, d := range active {
			snap.Dispatches.Agents = append(snap.Dispatches.Agents, dispatchToSummary(d))
		}
		snap.Dispatches.Active = len(active)
		snap.Dispatches.Total = len(active)
	}

	// Events
	if c.events != nil {
		var (
			evts []event.Event
			err  error
		)
		if opts.RunID != "" {
			evts, err = c.events.ListEvents(ctx, opts.RunID, 0, 0, 0, 0, opts.EventLimit)
		} else {
			evts, err = c.events.ListAllEvents(ctx, 0, 0, 0, 0, 0, opts.EventLimit)
		}
		if err != nil {
			return nil, fmt.Errorf("observation: list events: %w", err)
		}
		if evts != nil {
			snap.Events = evts
		}
	}

	// Queue
	if c.scheduler != nil {
		counts, err := c.scheduler.CountByStatus(ctx)
		if err != nil {
			return nil, fmt.Errorf("observation: count queue status: %w", err)
		}
		snap.Queue = QueueSummary{
			Pending:  counts["pending"],
			Running:  counts["running"],
			Retrying: counts["retrying"],
		}
	}

	// Budget (only when scoped to a run)
	if opts.RunID != "" && c.phases != nil && c.dispatches != nil {
		run, err := c.phases.Get(ctx, opts.RunID)
		if err != nil {
			return nil, fmt.Errorf("observation: get run for budget %s: %w", opts.RunID, err)
		}
		if run.TokenBudget != nil && *run.TokenBudget > 0 {
			agg, err := c.dispatches.AggregateTokens(ctx, opts.RunID)
			if err != nil {
				return nil, fmt.Errorf("observation: aggregate tokens: %w", err)
			}
			used := agg.TotalIn + agg.TotalOut
			budget := *run.TokenBudget
			snap.Budget = &BudgetSummary{
				RunID:     opts.RunID,
				Budget:    budget,
				Used:      used,
				Remaining: budget - used,
			}
		}
	}

	return snap, nil
}

// runToSummary converts a phase.Run to a RunSummary.
func runToSummary(r *phase.Run) RunSummary {
	return RunSummary{
		ID:         r.ID,
		Phase:      r.Phase,
		Status:     r.Status,
		ProjectDir: r.ProjectDir,
		Goal:       r.Goal,
		CreatedAt:  r.CreatedAt,
	}
}

// dispatchToSummary converts a dispatch.Dispatch to an AgentSummary.
func dispatchToSummary(d *dispatch.Dispatch) AgentSummary {
	s := AgentSummary{
		ID:        d.ID,
		AgentType: d.AgentType,
		Status:    d.Status,
		Turns:     d.Turns,
		InputTok:  d.InputTokens,
		OutputTok: d.OutputTokens,
	}
	if d.ScopeID != nil {
		s.ScopeID = *d.ScopeID
	}
	return s
}
