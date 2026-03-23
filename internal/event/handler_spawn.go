package event

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/mistakeknot/intercore/pkg/phase"
)

// AgentQuerier queries run agents. Decouples from runtrack package.
type AgentQuerier interface {
	ListPendingAgentIDs(ctx context.Context, runID string) ([]string, error)
}

// AgentSpawner triggers agent spawn by ID. Decouples from dispatch package.
type AgentSpawner interface {
	SpawnByAgentID(ctx context.Context, agentID string) error
}

// AgentSpawnerFunc adapts a plain function to the AgentSpawner interface.
type AgentSpawnerFunc func(ctx context.Context, agentID string) error

func (f AgentSpawnerFunc) SpawnByAgentID(ctx context.Context, agentID string) error {
	return f(ctx, agentID)
}

// NewSpawnHandler returns a handler that auto-spawns agents when phase
// reaches "executing". No-op for other phase transitions or dispatch events.
func NewSpawnHandler(querier AgentQuerier, spawner AgentSpawner, logger *slog.Logger) Handler {
	return func(ctx context.Context, e Event) error {
		if e.Source != SourcePhase || e.ToState != phase.Executing {
			return nil
		}

		agentIDs, err := querier.ListPendingAgentIDs(ctx, e.RunID)
		if err != nil {
			return fmt.Errorf("auto-spawn: list agents: %w", err)
		}

		if len(agentIDs) == 0 {
			return nil
		}

		for _, id := range agentIDs {
			if err := spawner.SpawnByAgentID(ctx, id); err != nil {
				if logger != nil {
					logger.WarnContext(ctx, "auto-spawn failed", "agent_id", id, "error", err)
				}
				continue
			}
			if logger != nil {
				logger.InfoContext(ctx, "auto-spawn started", "agent_id", id)
			}
		}
		return nil
	}
}
