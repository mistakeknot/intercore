package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	"github.com/mistakeknot/intercore/internal/cli"
	"github.com/mistakeknot/intercore/internal/phase"
	"github.com/mistakeknot/intercore/internal/runtrack"
)

// --- Run Commands ---

func cmdRun(ctx context.Context, args []string) int {
	if len(args) == 0 {
		slog.Error("run: missing subcommand", "expected", "create, status, advance, skip, rollback, phase, list, events, cancel, set, current, agent, artifact, action, tokens, budget, replay")
		return 3
	}

	switch args[0] {
	case "create":
		return cmdRunCreate(ctx, args[1:])
	case "status":
		return cmdRunStatus(ctx, args[1:])
	case "advance":
		return cmdRunAdvance(ctx, args[1:])
	case "skip":
		return cmdRunSkip(ctx, args[1:])
	case "phase":
		return cmdRunPhase(ctx, args[1:])
	case "list":
		return cmdRunList(ctx, args[1:])
	case "events":
		return cmdRunEvents(ctx, args[1:])
	case "rollback":
		return cmdRunRollback(ctx, args[1:])
	case "cancel":
		return cmdRunCancel(ctx, args[1:])
	case "set":
		return cmdRunSet(ctx, args[1:])
	case "current":
		return cmdRunCurrent(ctx, args[1:])
	case "agent":
		return cmdRunAgent(ctx, args[1:])
	case "artifact":
		return cmdRunArtifact(ctx, args[1:])
	case "action":
		return cmdRunAction(ctx, args[1:])
	case "tokens":
		return cmdRunTokens(ctx, args[1:])
	case "budget":
		return cmdRunBudget(ctx, args[1:])
	case "replay":
		return cmdRunReplay(ctx, args[1:])
	default:
		slog.Error("run: unknown subcommand", "subcommand", args[0])
		return 3
	}
}

func cmdRunList(ctx context.Context, args []string) int {
	f := cli.ParseFlags(args)
	activeOnly := f.Bool("active")
	portfolioOnly := f.Bool("portfolio")
	scopeFilter := f.StringPtr("scope")

	d, err := openDB()
	if err != nil {
		slog.Error("run list failed", "error", err)
		return 2
	}
	defer d.Close()

	store := phase.New(d.SqlDB())
	var runs []*phase.Run

	if activeOnly {
		runs, err = store.ListActive(ctx)
	} else {
		runs, err = store.List(ctx, scopeFilter)
	}
	if err != nil {
		slog.Error("run list failed", "error", err)
		return 2
	}

	// Filter to portfolio runs if requested
	if portfolioOnly {
		filtered := make([]*phase.Run, 0)
		for _, r := range runs {
			if r.ProjectDir == "" {
				filtered = append(filtered, r)
			}
		}
		runs = filtered
	}

	if flagJSON {
		items := make([]map[string]interface{}, len(runs))
		for i, r := range runs {
			items[i] = runToMap(r)
		}
		json.NewEncoder(os.Stdout).Encode(items)
	} else {
		for _, r := range runs {
			fmt.Printf("%s\t%s\t%s\t%s\n", r.ID, r.Status, r.Phase, r.Goal)
		}
	}
	return 0
}

// --- run output helpers ---

func runToMap(r *phase.Run) map[string]interface{} {
	m := map[string]interface{}{
		"id":           r.ID,
		"project_dir":  r.ProjectDir,
		"goal":         r.Goal,
		"status":       r.Status,
		"phase":        r.Phase,
		"complexity":   r.Complexity,
		"force_full":   r.ForceFull,
		"auto_advance": r.AutoAdvance,
		"created_at":   r.CreatedAt,
		"updated_at":   r.UpdatedAt,
	}
	if r.Phases != nil {
		m["phases"] = r.Phases
	}
	if r.CompletedAt != nil {
		m["completed_at"] = *r.CompletedAt
	}
	if r.ScopeID != nil {
		m["scope_id"] = *r.ScopeID
	}
	if r.TokenBudget != nil {
		m["token_budget"] = *r.TokenBudget
		m["budget_warn_pct"] = r.BudgetWarnPct
	}
	if r.Metadata != nil {
		m["metadata"] = *r.Metadata
	}
	if r.ParentRunID != nil {
		m["parent_run_id"] = *r.ParentRunID
	}
	if r.MaxDispatches > 0 {
		m["max_dispatches"] = r.MaxDispatches
	}
	if r.BudgetEnforce {
		m["budget_enforce"] = true
	}
	if r.MaxAgents > 0 {
		m["max_agents"] = r.MaxAgents
	}
	if r.GateRules != nil {
		m["gate_rules"] = r.GateRules
	}
	return m
}

func eventToMap(e *phase.PhaseEvent) map[string]interface{} {
	m := map[string]interface{}{
		"id":         e.ID,
		"run_id":     e.RunID,
		"from_phase": e.FromPhase,
		"to_phase":   e.ToPhase,
		"event_type": e.EventType,
		"created_at": e.CreatedAt,
	}
	if e.GateResult != nil {
		m["gate_result"] = *e.GateResult
	}
	if e.GateTier != nil {
		m["gate_tier"] = *e.GateTier
	}
	if e.Reason != nil {
		m["reason"] = *e.Reason
	}
	if e.EnvelopeJSON != nil && *e.EnvelopeJSON != "" {
		var envelope map[string]interface{}
		if err := json.Unmarshal([]byte(*e.EnvelopeJSON), &envelope); err == nil {
			m["envelope"] = envelope
		}
	}
	return m
}

func agentToMap(a *runtrack.Agent) map[string]interface{} {
	m := map[string]interface{}{
		"id":         a.ID,
		"run_id":     a.RunID,
		"agent_type": a.AgentType,
		"status":     a.Status,
		"created_at": a.CreatedAt,
		"updated_at": a.UpdatedAt,
	}
	if a.Name != nil {
		m["name"] = *a.Name
	}
	if a.DispatchID != nil {
		m["dispatch_id"] = *a.DispatchID
	}
	return m
}

func artifactToMap(a *runtrack.Artifact) map[string]interface{} {
	m := map[string]interface{}{
		"id":         a.ID,
		"run_id":     a.RunID,
		"phase":      a.Phase,
		"path":       a.Path,
		"type":       a.Type,
		"created_at": a.CreatedAt,
	}
	if a.ContentHash != nil {
		m["content_hash"] = *a.ContentHash
	}
	if a.DispatchID != nil {
		m["dispatch_id"] = *a.DispatchID
	}
	if a.Status != nil {
		m["status"] = *a.Status
	}
	return m
}

func printRun(r *phase.Run) {
	fmt.Printf("ID:         %s\n", r.ID)
	fmt.Printf("Status:     %s\n", r.Status)
	fmt.Printf("Phase:      %s\n", r.Phase)
	fmt.Printf("Goal:       %s\n", r.Goal)
	fmt.Printf("Project:    %s\n", r.ProjectDir)
	fmt.Printf("Complexity: %d\n", r.Complexity)
	if r.ForceFull {
		fmt.Printf("ForceFull:  true\n")
	}
	if !r.AutoAdvance {
		fmt.Printf("AutoAdv:    false\n")
	}
	if r.ScopeID != nil {
		fmt.Printf("Scope:      %s\n", *r.ScopeID)
	}
	if r.TokenBudget != nil {
		fmt.Printf("Budget:     %d tokens (warn at %d%%)\n", *r.TokenBudget, r.BudgetWarnPct)
	}
	if r.ParentRunID != nil {
		fmt.Printf("Parent:     %s\n", *r.ParentRunID)
	}
	if r.MaxDispatches > 0 {
		fmt.Printf("MaxDisp:    %d\n", r.MaxDispatches)
	}
}
