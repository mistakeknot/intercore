package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/mistakeknot/intercore/internal/action"
	"github.com/mistakeknot/intercore/internal/cli"
	"github.com/mistakeknot/intercore/internal/phase"
)

func cmdRunCreate(ctx context.Context, args []string) int {
	f := cli.ParseFlags(args)

	project := f.String("project", "")
	projects := f.String("projects", "")
	goal := f.String("goal", "")
	scopeID := f.String("scope-id", "")
	phasesJSON := f.String("phases", "")
	actionsJSON := f.String("actions", "")
	gatesJSON := f.String("gates", "")
	gatesFile := f.String("gates-file", "")
	budgetEnforce := f.Bool("budget-enforce")

	complexity, err := f.Int("complexity", 3)
	if err != nil || complexity < 1 || complexity > 5 {
		slog.Error("run create: invalid complexity", "value", f.String("complexity", ""))
		return 3
	}

	tokenBudget, err := f.Int64("token-budget", 0)
	if err != nil || (f.Has("token-budget") && tokenBudget <= 0) {
		slog.Error("run create: invalid token-budget", "value", f.String("token-budget", ""))
		return 3
	}

	budgetWarnPct, err := f.Int("budget-warn-pct", 80)
	if err != nil || (f.Has("budget-warn-pct") && (budgetWarnPct < 1 || budgetWarnPct > 99)) {
		slog.Error("run create: invalid budget-warn-pct", "value", f.String("budget-warn-pct", ""))
		return 3
	}

	maxDispatches, err := f.Int("max-dispatches", 0)
	if err != nil || (f.Has("max-dispatches") && maxDispatches < 0) {
		slog.Error("run create: invalid max-dispatches", "value", f.String("max-dispatches", ""))
		return 3
	}

	maxAgents, err := f.Int("max-agents", 0)
	if err != nil || (f.Has("max-agents") && maxAgents < 0) {
		slog.Error("run create: invalid max-agents", "value", f.String("max-agents", ""))
		return 3
	}

	if goal == "" {
		slog.Error("run create: --goal is required")
		return 3
	}

	if projects != "" && project != "" {
		slog.Error("run create: --project and --projects are mutually exclusive")
		return 3
	}

	if gatesJSON != "" && gatesFile != "" {
		slog.Error("run create: --gates and --gates-file are mutually exclusive")
		return 3
	}

	// Parse gate rules (fail-fast before DB)
	var gateRules map[string][]phase.SpecGateRule
	if gatesFile != "" {
		data, err := os.ReadFile(gatesFile)
		if err != nil {
			slog.Error("run create: read gates file", "error", err)
			return 2
		}
		gatesJSON = string(data)
	}
	if gatesJSON != "" {
		var err error
		gateRules, err = phase.ParseGateRules(gatesJSON)
		if err != nil {
			slog.Error("run create failed", "error", err)
			return 3
		}
	}

	d, err := openDB()
	if err != nil {
		slog.Error("run create failed", "error", err)
		return 2
	}
	defer d.Close()

	// Validate custom phases if provided
	var customPhases []string
	if phasesJSON != "" {
		parsed, err := phase.ParsePhaseChain(phasesJSON)
		if err != nil {
			slog.Error("run create failed", "error", err)
			return 3
		}
		customPhases = parsed
	}
	activeChain := phase.DefaultPhaseChain
	if len(customPhases) > 0 {
		activeChain = customPhases
	}
	if gateRules != nil {
		if err := phase.ValidateGateRulesForChain(activeChain, gateRules); err != nil {
			slog.Error("run create failed", "error", err)
			return 3
		}
	}

	store := phase.New(d.SqlDB())

	// Portfolio mode: create parent + children
	if projects != "" {
		projectPaths := strings.Split(projects, ",")
		if len(projectPaths) < 2 {
			slog.Error("run create: --projects requires at least 2 comma-separated paths")
			return 3
		}
		// Resolve to absolute paths
		for i, p := range projectPaths {
			abs, err := filepath.Abs(strings.TrimSpace(p))
			if err != nil {
				slog.Error("run create: invalid project path", "value", p, "error", err)
				return 3
			}
			projectPaths[i] = abs
		}

		portfolio := &phase.Run{
			Goal:          goal,
			Complexity:    complexity,
			AutoAdvance:   true,
			BudgetWarnPct: budgetWarnPct,
			Phases:        customPhases,
			MaxDispatches: maxDispatches,
			BudgetEnforce: budgetEnforce,
			MaxAgents:     maxAgents,
			GateRules:     gateRules,
		}
		if tokenBudget > 0 {
			portfolio.TokenBudget = &tokenBudget
		}
		if scopeID != "" {
			portfolio.ScopeID = &scopeID
		}

		children := make([]*phase.Run, len(projectPaths))
		for i, p := range projectPaths {
			children[i] = &phase.Run{
				ProjectDir:    p,
				Goal:          goal,
				Complexity:    complexity,
				AutoAdvance:   true,
				BudgetWarnPct: budgetWarnPct,
				Phases:        customPhases,
			}
		}

		portfolioID, childIDs, err := store.CreatePortfolio(ctx, portfolio, children)
		if err != nil {
			slog.Error("run create failed", "error", err)
			return 2
		}

		if flagJSON {
			json.NewEncoder(os.Stdout).Encode(map[string]interface{}{
				"id":       portfolioID,
				"children": childIDs,
			})
		} else {
			fmt.Println(portfolioID)
			for _, cid := range childIDs {
				fmt.Printf("  child: %s\n", cid)
			}
		}
		return 0
	}

	// Single project mode
	if project == "" {
		cwd, err := os.Getwd()
		if err != nil {
			slog.Error("run create: cannot determine project dir", "error", err)
			return 2
		}
		project = cwd
	}

	run := &phase.Run{
		ProjectDir:    project,
		Goal:          goal,
		Complexity:    complexity,
		AutoAdvance:   true,
		BudgetWarnPct: budgetWarnPct,
		Phases:        customPhases,
		BudgetEnforce: budgetEnforce,
		MaxAgents:     maxAgents,
		GateRules:     gateRules,
	}
	if tokenBudget > 0 {
		run.TokenBudget = &tokenBudget
	}
	if scopeID != "" {
		run.ScopeID = &scopeID
	}

	// Parse --actions JSON before creating the run (fail-fast, no orphaned runs)
	var actionBatch map[string]*action.Action
	if actionsJSON != "" {
		var actionMap map[string]struct {
			Command string  `json:"command"`
			Args    *string `json:"args,omitempty"`
			Mode    string  `json:"mode,omitempty"`
			Type    string  `json:"type,omitempty"`
		}
		if err := json.Unmarshal([]byte(actionsJSON), &actionMap); err != nil {
			slog.Error("run create: invalid --actions JSON", "error", err)
			return 2
		}
		actionBatch = make(map[string]*action.Action, len(actionMap))
		for aPhase, spec := range actionMap {
			a := &action.Action{
				Command:    spec.Command,
				ActionType: spec.Type,
				Mode:       spec.Mode,
				Args:       spec.Args,
			}
			actionBatch[aPhase] = a
		}
	}

	id, err := store.Create(ctx, run)
	if err != nil {
		slog.Error("run create failed", "error", err)
		return 2
	}

	// Register phase actions after run creation (JSON already validated above)
	if actionBatch != nil {
		aStore := action.New(d.SqlDB())
		if err := aStore.AddBatch(ctx, id, actionBatch); err != nil {
			slog.Error("run create: register actions", "error", err)
			return 2
		}
	}

	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(map[string]interface{}{
			"id":    id,
			"phase": phase.PhaseBrainstorm,
		})
	} else {
		fmt.Println(id)
	}
	return 0
}
