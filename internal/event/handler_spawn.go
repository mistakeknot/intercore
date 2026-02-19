package event

import (
	"context"
	"fmt"
	"io"
	"os"
)

// AgentQuerier queries run agents. Decouples from runtrack package.
type AgentQuerier interface {
	ListPendingAgentIDs(ctx context.Context, runID string) ([]string, error)
}

// AgentSpawner triggers agent spawn by ID. Decouples from dispatch package.
type AgentSpawner interface {
	SpawnByAgentID(ctx context.Context, agentID string) error
}

// NewSpawnHandler returns a handler that auto-spawns agents when phase
// reaches "executing". No-op for other phase transitions or dispatch events.
func NewSpawnHandler(querier AgentQuerier, spawner AgentSpawner, logw io.Writer) Handler {
	if logw == nil {
		logw = os.Stderr
	}
	return func(ctx context.Context, e Event) error {
		if e.Source != SourcePhase || e.ToState != "executing" {
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
				fmt.Fprintf(logw, "[event] auto-spawn: agent %s failed: %v\n", id, err)
				continue
			}
			fmt.Fprintf(logw, "[event] auto-spawn: agent %s started\n", id)
		}
		return nil
	}
}
