package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"

	"github.com/mistakeknot/intercore/internal/budget"
	"github.com/mistakeknot/intercore/internal/cli"
	"github.com/mistakeknot/intercore/internal/dispatch"
	"github.com/mistakeknot/intercore/internal/phase"
	"github.com/mistakeknot/intercore/internal/runtrack"
	"github.com/mistakeknot/intercore/internal/state"
)

func cmdRunSet(ctx context.Context, args []string) int {
	f := cli.ParseFlags(args)

	if len(f.Positionals) < 1 {
		fmt.Fprintf(os.Stderr, "ic: run set: usage: ic run set <id> [--complexity=N] [--auto-advance=bool] [--force-full=bool] [--max-dispatches=N]\n")
		return 3
	}
	id := f.Positionals[0]

	var complexity *int
	if f.Has("complexity") {
		c, err := f.Int("complexity", 0)
		if err != nil || c < 1 || c > 5 {
			slog.Error("run set: invalid complexity", "value", f.String("complexity", ""))
			return 3
		}
		complexity = &c
	}

	var autoAdvance *bool
	if raw, ok := f.Raw("auto-advance"); ok {
		b, err := strconv.ParseBool(raw)
		if err != nil {
			slog.Error("run set: invalid bool", "value", raw)
			return 3
		}
		autoAdvance = &b
	}

	var forceFull *bool
	if raw, ok := f.Raw("force-full"); ok {
		b, err := strconv.ParseBool(raw)
		if err != nil {
			slog.Error("run set: invalid bool", "value", raw)
			return 3
		}
		forceFull = &b
	}

	var maxDispatches *int
	if f.Has("max-dispatches") {
		v, err := f.Int("max-dispatches", 0)
		if err != nil || v < 0 {
			slog.Error("run set: invalid max-dispatches", "value", f.String("max-dispatches", ""))
			return 3
		}
		maxDispatches = &v
	}

	if complexity == nil && autoAdvance == nil && forceFull == nil && maxDispatches == nil {
		slog.Error("run set: no settings to update")
		return 3
	}

	d, err := openDB()
	if err != nil {
		slog.Error("run set failed", "error", err)
		return 2
	}
	defer d.Close()

	store := phase.New(d.SqlDB())

	// Get current state for the event record
	run, err := store.Get(ctx, id)
	if err != nil {
		if err == phase.ErrNotFound {
			slog.Error("run set: not found", "id", id)
			return 1
		}
		slog.Error("run set failed", "error", err)
		return 2
	}

	// --max-dispatches is only valid for portfolio runs
	if maxDispatches != nil {
		if run.ProjectDir != "" {
			slog.Error("run set: --max-dispatches is only valid for portfolio runs")
			return 3
		}
		if err := store.UpdateMaxDispatches(ctx, id, *maxDispatches); err != nil {
			slog.Error("run set failed", "error", err)
			return 2
		}
	}

	if complexity != nil || autoAdvance != nil || forceFull != nil {
		if err := store.UpdateSettings(ctx, id, complexity, autoAdvance, forceFull); err != nil {
			slog.Error("run set failed", "error", err)
			return 2
		}
	}

	// Record set event
	store.AddEvent(ctx, &phase.PhaseEvent{
		RunID:     id,
		FromPhase: run.Phase,
		ToPhase:   run.Phase,
		EventType: phase.EventSet,
	})

	fmt.Println("updated")
	return 0
}
func cmdRunAgent(ctx context.Context, args []string) int {
	if len(args) == 0 {
		slog.Error("run agent: missing subcommand", "expected", "add, list, update")
		return 3
	}

	switch args[0] {
	case "add":
		return cmdRunAgentAdd(ctx, args[1:])
	case "list":
		return cmdRunAgentList(ctx, args[1:])
	case "update":
		return cmdRunAgentUpdate(ctx, args[1:])
	default:
		slog.Error("run agent: unknown subcommand", "subcommand", args[0])
		return 3
	}
}

func cmdRunAgentAdd(ctx context.Context, args []string) int {
	f := cli.ParseFlags(args)
	agentType := f.String("type", "")
	name := f.String("name", "")
	dispatchID := f.String("dispatch-id", "")

	if len(f.Positionals) < 1 {
		fmt.Fprintf(os.Stderr, "ic: run agent add: usage: ic run agent add <run_id> --type=<type> [--name=<name>] [--dispatch-id=<id>]\n")
		return 3
	}
	runID := f.Positionals[0]

	if agentType == "" {
		agentType = "claude"
	}

	d, err := openDB()
	if err != nil {
		slog.Error("run agent add failed", "error", err)
		return 2
	}
	defer d.Close()

	agent := &runtrack.Agent{
		RunID:     runID,
		AgentType: agentType,
	}
	if name != "" {
		agent.Name = &name
	}
	if dispatchID != "" {
		agent.DispatchID = &dispatchID
	}

	store := runtrack.New(d.SqlDB())
	id, err := store.AddAgent(ctx, agent)
	if err != nil {
		if err == runtrack.ErrRunNotFound {
			slog.Error("run agent add: run not found", "id", runID)
			return 1
		}
		slog.Error("run agent add failed", "error", err)
		return 2
	}

	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(map[string]interface{}{
			"id": id,
		})
	} else {
		fmt.Println(id)
	}
	return 0
}

func cmdRunAgentList(ctx context.Context, args []string) int {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "ic: run agent list: usage: ic run agent list <run_id>\n")
		return 3
	}
	runID := args[0]

	d, err := openDB()
	if err != nil {
		slog.Error("run agent list failed", "error", err)
		return 2
	}
	defer d.Close()

	store := runtrack.New(d.SqlDB())
	agents, err := store.ListAgents(ctx, runID)
	if err != nil {
		slog.Error("run agent list failed", "error", err)
		return 2
	}

	if flagJSON {
		items := make([]map[string]interface{}, len(agents))
		for i, a := range agents {
			items[i] = agentToMap(a)
		}
		json.NewEncoder(os.Stdout).Encode(items)
	} else {
		for _, a := range agents {
			name := ""
			if a.Name != nil {
				name = *a.Name
			}
			fmt.Printf("%s\t%s\t%s\t%s\n", a.ID, a.AgentType, a.Status, name)
		}
	}
	return 0
}

func cmdRunAgentUpdate(ctx context.Context, args []string) int {
	f := cli.ParseFlags(args)
	status := f.String("status", "")

	if len(f.Positionals) < 1 {
		fmt.Fprintf(os.Stderr, "ic: run agent update: usage: ic run agent update <agent_id> --status=<status>\n")
		return 3
	}
	agentID := f.Positionals[0]

	if status == "" {
		slog.Error("run agent update: --status is required")
		return 3
	}

	switch status {
	case runtrack.StatusActive, runtrack.StatusCompleted, runtrack.StatusFailed:
		// valid
	default:
		slog.Error("run agent update: invalid status (must be active, completed, or failed)", "status", status)
		return 3
	}

	d, err := openDB()
	if err != nil {
		slog.Error("run agent update failed", "error", err)
		return 2
	}
	defer d.Close()

	store := runtrack.New(d.SqlDB())
	if err := store.UpdateAgent(ctx, agentID, status); err != nil {
		if err == runtrack.ErrAgentNotFound {
			slog.Error("run agent update: not found", "id", agentID)
			return 1
		}
		slog.Error("run agent update failed", "error", err)
		return 2
	}

	fmt.Println("updated")
	return 0
}
func cmdRunTokens(ctx context.Context, args []string) int {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "ic: run tokens: usage: ic run tokens <run_id>\n")
		return 3
	}
	runID := args[0]

	d, err := openDB()
	if err != nil {
		slog.Error("run tokens failed", "error", err)
		return 2
	}
	defer d.Close()

	dStore := dispatch.New(d.SqlDB(), nil)
	agg, err := dStore.AggregateTokens(ctx, runID)
	if err != nil {
		slog.Error("run tokens failed", "error", err)
		return 2
	}

	total := agg.TotalIn + agg.TotalOut
	var cacheRatio float64
	if agg.TotalIn > 0 {
		cacheRatio = float64(agg.TotalCache) / float64(agg.TotalIn+agg.TotalCache) * 100
	}

	if flagJSON {
		out := map[string]interface{}{
			"run_id":        runID,
			"input_tokens":  agg.TotalIn,
			"output_tokens": agg.TotalOut,
			"cache_hits":    agg.TotalCache,
			"total_tokens":  total,
		}
		if agg.TotalIn > 0 || agg.TotalCache > 0 {
			out["cache_ratio"] = cacheRatio
		}
		json.NewEncoder(os.Stdout).Encode(out)
	} else {
		fmt.Printf("Run: %s\n", runID)
		fmt.Printf("  Input tokens:  %d\n", agg.TotalIn)
		fmt.Printf("  Output tokens: %d\n", agg.TotalOut)
		fmt.Printf("  Cache hits:    %d\n", agg.TotalCache)
		if agg.TotalIn > 0 || agg.TotalCache > 0 {
			fmt.Printf("  Cache ratio:   %.1f%%\n", cacheRatio)
		}
		fmt.Printf("  Total tokens:  %d\n", total)
	}
	return 0
}

func cmdRunBudget(ctx context.Context, args []string) int {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "ic: run budget: usage: ic run budget <run_id>\n")
		return 3
	}
	runID := args[0]

	d, err := openDB()
	if err != nil {
		slog.Error("run budget failed", "error", err)
		return 2
	}
	defer d.Close()

	pStore := phase.New(d.SqlDB())
	dStore := dispatch.New(d.SqlDB(), nil)
	sStore := state.New(d.SqlDB())

	checker := budget.New(pStore, dStore, sStore, nil)
	result, err := checker.Check(ctx, runID)
	if err != nil {
		slog.Error("run budget failed", "error", err)
		return 2
	}
	if result == nil {
		if flagJSON {
			json.NewEncoder(os.Stdout).Encode(map[string]interface{}{
				"run_id":  runID,
				"budget":  nil,
				"message": "no budget set",
			})
		} else {
			fmt.Printf("Run %s: no budget set\n", runID)
		}
		return 0
	}

	pct := float64(0)
	if result.Budget > 0 {
		pct = float64(result.Used) / float64(result.Budget) * 100
	}

	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(map[string]interface{}{
			"run_id":    result.RunID,
			"budget":    result.Budget,
			"used":      result.Used,
			"warn_pct":  result.WarnPct,
			"usage_pct": pct,
			"warning":   result.Warning,
			"exceeded":  result.Exceeded,
		})
	} else {
		fmt.Printf("Run: %s\n", result.RunID)
		fmt.Printf("  Budget:    %d tokens\n", result.Budget)
		fmt.Printf("  Used:      %d tokens (%.1f%%)\n", result.Used, pct)
		fmt.Printf("  Warn at:   %d%%\n", result.WarnPct)
		if result.Exceeded {
			fmt.Printf("  Status:    EXCEEDED\n")
		} else if result.Warning {
			fmt.Printf("  Status:    WARNING\n")
		} else {
			fmt.Printf("  Status:    OK\n")
		}
	}

	if result.Exceeded {
		return 1
	}
	return 0
}
func cmdRunArtifact(ctx context.Context, args []string) int {
	if len(args) == 0 {
		slog.Error("run artifact: missing subcommand", "expected", "add, list")
		return 3
	}

	switch args[0] {
	case "add":
		return cmdRunArtifactAdd(ctx, args[1:])
	case "list":
		return cmdRunArtifactList(ctx, args[1:])
	default:
		slog.Error("run artifact: unknown subcommand", "subcommand", args[0])
		return 3
	}
}

func cmdRunArtifactAdd(ctx context.Context, args []string) int {
	f := cli.ParseFlags(args)
	artifactPhase := f.String("phase", "")
	path := f.String("path", "")
	artifactType := f.String("type", "")
	dispatch := f.String("dispatch", "")

	if len(f.Positionals) < 1 {
		fmt.Fprintf(os.Stderr, "ic: run artifact add: usage: ic run artifact add <run_id> --phase=<phase> --path=<path> [--type=<type>] [--dispatch=<id>]\n")
		return 3
	}
	runID := f.Positionals[0]

	if artifactPhase == "" {
		slog.Error("run artifact add: --phase is required")
		return 3
	}
	if path == "" {
		slog.Error("run artifact add: --path is required")
		return 3
	}
	if artifactType == "" {
		artifactType = "file"
	}

	d, err := openDB()
	if err != nil {
		slog.Error("run artifact add failed", "error", err)
		return 2
	}
	defer d.Close()

	artifact := &runtrack.Artifact{
		RunID: runID,
		Phase: artifactPhase,
		Path:  path,
		Type:  artifactType,
	}
	if dispatch != "" {
		artifact.DispatchID = &dispatch
	}

	store := runtrack.New(d.SqlDB())
	id, err := store.AddArtifact(ctx, artifact)
	if err != nil {
		if err == runtrack.ErrRunNotFound {
			slog.Error("run artifact add: run not found", "id", runID)
			return 1
		}
		slog.Error("run artifact add failed", "error", err)
		return 2
	}

	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(map[string]interface{}{
			"id": id,
		})
	} else {
		fmt.Println(id)
	}
	return 0
}

func cmdRunArtifactList(ctx context.Context, args []string) int {
	f := cli.ParseFlags(args)
	phaseFilter := f.StringPtr("phase")

	if len(f.Positionals) < 1 {
		fmt.Fprintf(os.Stderr, "ic: run artifact list: usage: ic run artifact list <run_id> [--phase=<phase>]\n")
		return 3
	}
	runID := f.Positionals[0]

	d, err := openDB()
	if err != nil {
		slog.Error("run artifact list failed", "error", err)
		return 2
	}
	defer d.Close()

	store := runtrack.New(d.SqlDB())
	artifacts, err := store.ListArtifacts(ctx, runID, phaseFilter)
	if err != nil {
		slog.Error("run artifact list failed", "error", err)
		return 2
	}

	if flagJSON {
		items := make([]map[string]interface{}, len(artifacts))
		for i, a := range artifacts {
			items[i] = artifactToMap(a)
		}
		json.NewEncoder(os.Stdout).Encode(items)
	} else {
		for _, a := range artifacts {
			fmt.Printf("%s\t%s\t%s\t%s\n", a.ID, a.Phase, a.Type, a.Path)
		}
	}
	return 0
}
