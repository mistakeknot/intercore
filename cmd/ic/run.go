package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mistakeknot/intercore/internal/action"
	"github.com/mistakeknot/intercore/internal/budget"
	"github.com/mistakeknot/intercore/internal/dispatch"
	"github.com/mistakeknot/intercore/internal/event"
	"github.com/mistakeknot/intercore/internal/phase"
	portfoliopkg "github.com/mistakeknot/intercore/internal/portfolio"
	"github.com/mistakeknot/intercore/internal/replay"
	"github.com/mistakeknot/intercore/internal/runtrack"
	"github.com/mistakeknot/intercore/internal/state"
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

func cmdRunCreate(ctx context.Context, args []string) int {
	var project, goal, scopeID, phasesJSON, projects, actionsJSON string
	var gatesJSON, gatesFile string
	complexity := 3
	var tokenBudget int64
	var maxDispatches, maxAgents int
	var budgetEnforce bool
	budgetWarnPct := 80

	for i := 0; i < len(args); i++ {
		switch {
		case strings.HasPrefix(args[i], "--project="):
			project = strings.TrimPrefix(args[i], "--project=")
		case strings.HasPrefix(args[i], "--projects="):
			projects = strings.TrimPrefix(args[i], "--projects=")
		case strings.HasPrefix(args[i], "--goal="):
			goal = strings.TrimPrefix(args[i], "--goal=")
		case strings.HasPrefix(args[i], "--complexity="):
			val := strings.TrimPrefix(args[i], "--complexity=")
			c, err := strconv.Atoi(val)
			if err != nil || c < 1 || c > 5 {
				slog.Error("run create: invalid complexity", "value", val)
				return 3
			}
			complexity = c
		case strings.HasPrefix(args[i], "--scope-id="):
			scopeID = strings.TrimPrefix(args[i], "--scope-id=")
		case strings.HasPrefix(args[i], "--phases="):
			phasesJSON = strings.TrimPrefix(args[i], "--phases=")
		case strings.HasPrefix(args[i], "--token-budget="):
			val := strings.TrimPrefix(args[i], "--token-budget=")
			v, err := strconv.ParseInt(val, 10, 64)
			if err != nil || v <= 0 {
				slog.Error("run create: invalid token-budget", "value", val)
				return 3
			}
			tokenBudget = v
		case strings.HasPrefix(args[i], "--budget-warn-pct="):
			val := strings.TrimPrefix(args[i], "--budget-warn-pct=")
			v, err := strconv.Atoi(val)
			if err != nil || v < 1 || v > 99 {
				slog.Error("run create: invalid budget-warn-pct", "value", val)
				return 3
			}
			budgetWarnPct = v
		case strings.HasPrefix(args[i], "--max-dispatches="):
			val := strings.TrimPrefix(args[i], "--max-dispatches=")
			v, err := strconv.Atoi(val)
			if err != nil || v < 0 {
				slog.Error("run create: invalid max-dispatches", "value", val)
				return 3
			}
			maxDispatches = v
		case args[i] == "--budget-enforce":
			budgetEnforce = true
		case strings.HasPrefix(args[i], "--max-agents="):
			val := strings.TrimPrefix(args[i], "--max-agents=")
			v, err := strconv.Atoi(val)
			if err != nil || v < 0 {
				slog.Error("run create: invalid max-agents", "value", val)
				return 3
			}
			maxAgents = v
		case strings.HasPrefix(args[i], "--actions="):
			actionsJSON = strings.TrimPrefix(args[i], "--actions=")
		case strings.HasPrefix(args[i], "--gates="):
			gatesJSON = strings.TrimPrefix(args[i], "--gates=")
		case strings.HasPrefix(args[i], "--gates-file="):
			gatesFile = strings.TrimPrefix(args[i], "--gates-file=")
		default:
			slog.Error("run create: unknown flag", "value", args[i])
			return 3
		}
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

func cmdRunStatus(ctx context.Context, args []string) int {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "ic: run status: usage: ic run status <id>\n")
		return 3
	}

	d, err := openDB()
	if err != nil {
		slog.Error("run status failed", "error", err)
		return 2
	}
	defer d.Close()

	store := phase.New(d.SqlDB())
	run, err := store.Get(ctx, args[0])
	if err != nil {
		if err == phase.ErrNotFound {
			slog.Error("run status: not found", "id", args[0])
			return 1
		}
		slog.Error("run status failed", "error", err)
		return 2
	}

	// Check for children (portfolio run)
	children, _ := store.GetChildren(ctx, args[0])

	if flagJSON {
		m := runToMap(run)
		if len(children) > 0 {
			childMaps := make([]map[string]interface{}, len(children))
			for i, c := range children {
				childMaps[i] = runToMap(c)
			}
			m["children"] = childMaps
		}
		json.NewEncoder(os.Stdout).Encode(m)
	} else {
		printRun(run)
		if len(children) > 0 {
			fmt.Printf("\nChildren: (%d)\n", len(children))
			for _, c := range children {
				fmt.Printf("  %s\t%s\t%s\t%s\t%s\n", c.ID, c.Status, c.Phase, c.ProjectDir, c.Goal)
			}
		}
	}
	return 0
}

func cmdRunAdvance(ctx context.Context, args []string) int {
	var id string
	priority := 1 // TierHard: evaluate AND block on gate failure (was 4/TierNone: skip all gates)
	disableGates := false
	skipReason := ""
	calibrationFile := ""

	var positional []string
	for i := 0; i < len(args); i++ {
		switch {
		case strings.HasPrefix(args[i], "--priority="):
			val := strings.TrimPrefix(args[i], "--priority=")
			p, err := strconv.Atoi(val)
			if err != nil {
				slog.Error("run advance: invalid priority", "value", val)
				return 3
			}
			priority = p
		case args[i] == "--disable-gates":
			disableGates = true
		case strings.HasPrefix(args[i], "--skip-reason="):
			skipReason = strings.TrimPrefix(args[i], "--skip-reason=")
		case strings.HasPrefix(args[i], "--calibration-file="):
			calibrationFile = strings.TrimPrefix(args[i], "--calibration-file=")
		default:
			positional = append(positional, args[i])
		}
	}

	if len(positional) < 1 {
		fmt.Fprintf(os.Stderr, "ic: run advance: usage: ic run advance <id> [--priority=N] [--disable-gates] [--skip-reason=S]\n")
		return 3
	}
	id = positional[0]

	d, err := openDB()
	if err != nil {
		slog.Error("run advance failed", "error", err)
		return 2
	}
	defer d.Close()

	store := phase.New(d.SqlDB())
	rtStore := runtrack.New(d.SqlDB())
	evStore := event.NewStore(d.SqlDB())

	// Set up event notifier with handlers
	notifier := event.NewNotifier()
	var eventLogger *slog.Logger
	if flagVerbose {
		eventLogger = slog.Default()
	}
	notifier.Subscribe("log", event.NewLogHandler(eventLogger))

	// Get run info for project dir (needed by hook handler)
	run, err := store.Get(ctx, id)
	if err != nil {
		if err == phase.ErrNotFound {
			slog.Error("run advance: not found", "id", id)
			return 1
		}
		slog.Error("run advance failed", "error", err)
		return 2
	}
	notifier.Subscribe("hook", event.NewHookHandler(run.ProjectDir, slog.Default()))

	// Dispatch event recorder: writes to dispatch_events + notifies
	dispatchRecorder := func(dispatchID, runID, fromStatus, toStatus string) {
		e := event.Event{
			RunID:     runID,
			Source:    event.SourceDispatch,
			Type:      "status_change",
			FromState: fromStatus,
			ToState:   toStatus,
			Timestamp: time.Now(),
		}
		if err := evStore.AddDispatchEvent(ctx, dispatchID, runID, fromStatus, toStatus, "status_change", "", nil); err != nil {
			slog.Debug("event: dispatch event", "error", err)
		}
		notifier.Notify(ctx, e)
	}
	dStore := dispatch.New(d.SqlDB(), dispatchRecorder)

	// Auto-spawn adapter: looks up agent, re-uses dispatch config or convention path
	spawner := event.AgentSpawnerFunc(func(ctx context.Context, agentID string) error {
		agent, err := rtStore.GetAgent(ctx, agentID)
		if err != nil {
			return fmt.Errorf("spawn lookup: %w", err)
		}

		var opts dispatch.SpawnOptions
		opts.ProjectDir = run.ProjectDir
		opts.AgentType = agent.AgentType

		// If agent has a prior dispatch, re-use its spawn config
		if agent.DispatchID != nil {
			prior, err := dStore.Get(ctx, *agent.DispatchID)
			if err != nil {
				return fmt.Errorf("spawn: lookup prior dispatch %s: %w", *agent.DispatchID, err)
			}
			if prior.PromptFile != nil {
				opts.PromptFile = *prior.PromptFile
			}
			if prior.Model != nil {
				opts.Model = *prior.Model
			}
			if prior.Sandbox != nil {
				opts.Sandbox = *prior.Sandbox
			}
		}

		// Fallback: convention-based prompt path
		if opts.PromptFile == "" && agent.Name != nil {
			opts.PromptFile = filepath.Join(run.ProjectDir, ".ic", "prompts", *agent.Name+".md")
		}

		if opts.PromptFile == "" {
			return fmt.Errorf("spawn: agent %s has no prompt file and no name for convention lookup", agentID)
		}

		spawnResult, err := dispatch.Spawn(ctx, dStore, opts)
		if err != nil {
			return fmt.Errorf("spawn: agent %s: %w", agentID, err)
		}

		// Link the new dispatch back to the agent record (CAS: only if not already linked)
		if err := rtStore.UpdateAgentDispatch(ctx, agentID, spawnResult.ID); err != nil {
			// Kill the orphan process to prevent resource leak
			if spawnResult.Cmd != nil && spawnResult.Cmd.Process != nil {
				_ = spawnResult.Cmd.Process.Kill()
			}
			return fmt.Errorf("spawn: link dispatch to agent %s: %w", agentID, err)
		}

		return nil
	})
	notifier.Subscribe("spawn", event.NewSpawnHandler(rtStore, spawner, slog.Default()))

	// Phase event callback: notifies after phase transition
	phaseCallback := func(runID, eventType, fromPhase, toPhase, reason string) {
		e := event.Event{
			RunID:     runID,
			Source:    event.SourcePhase,
			Type:      eventType,
			FromState: fromPhase,
			ToState:   toPhase,
			Reason:    reason,
			Timestamp: time.Now(),
		}
		notifier.Notify(ctx, e)
	}

	// Wire DepQuerier for child runs with dependencies
	var dq phase.DepQuerier
	if run.ParentRunID != nil && *run.ParentRunID != "" {
		dq = portfoliopkg.NewDepStore(d.SqlDB())
	}

	// Budget gate querier: only needed if run has budget enforcement
	var bq phase.BudgetQuerier
	if run.BudgetEnforce {
		sStore := state.New(d.SqlDB())
		checker := budget.New(store, dStore, sStore, nil)
		bq = &cliBudgetQuerier{checker: checker}
	}

	// Resolve spec-defined gate rules from agency specs (if loaded)
	var specRules []phase.SpecGateRule
	{
		sStore := state.New(d.SqlDB())
		gateKey := fmt.Sprintf("agency.gates.%s", run.Phase)
		gateJSON, gerr := sStore.Get(ctx, gateKey, id)
		if gerr == nil && gateJSON != nil {
			var specGates struct {
				Exit []struct {
					Check string `json:"check"`
					Phase string `json:"phase,omitempty"`
					Tier  string `json:"tier"`
				} `json:"exit"`
			}
			if json.Unmarshal(gateJSON, &specGates) == nil {
				for _, sg := range specGates.Exit {
					specRules = append(specRules, phase.SpecGateRule{
						Check: sg.Check,
						Phase: sg.Phase,
						Tier:  sg.Tier,
					})
				}
			}
		}
	}

	// Load calibrated tiers from file (if provided)
	var calibratedTiers map[string]string
	if calibrationFile != "" {
		var loadErr error
		calibratedTiers, loadErr = LoadGateCalibration(calibrationFile)
		if loadErr != nil {
			slog.Error("run advance: calibration file", "error", loadErr)
			return 2
		}
		emitCalibrationStaleEvent(calibrationFile)
	}

	result, err := phase.Advance(ctx, store, id, phase.GateConfig{
		Priority:        priority,
		DisableAll:      disableGates,
		SkipReason:      skipReason,
		SpecRules:       specRules,
		CalibratedTiers: calibratedTiers,
	}, rtStore, dStore, store, dq, bq, phaseCallback)
	if err != nil {
		if err == phase.ErrNotFound {
			slog.Error("run advance: not found", "id", id)
			return 1
		}
		if err == phase.ErrTerminalRun || err == phase.ErrTerminalPhase {
			slog.Error("run advance failed", "error", err)
			return 1
		}
		slog.Error("run advance failed", "error", err)
		return 2
	}

	// Resolve actions for the destination phase
	var resolvedActions []*action.Action
	if result.Advanced {
		aStore := action.New(d.SqlDB())
		var actErr error
		resolvedActions, actErr = aStore.ListForPhaseResolved(ctx, id, result.ToPhase, run.ProjectDir)
		if actErr != nil {
			slog.Warn("run advance: action resolution failed", "error", actErr)
		}
	}

	// Enrich response with context about the destination phase.
	var activeAgentCount int
	var nextGateRequirements []string
	if result.Advanced {
		// Count active agents
		agents, agentErr := rtStore.ListAgents(ctx, id)
		if agentErr == nil {
			for _, ag := range agents {
				if ag.Status == "active" || ag.Status == "running" {
					activeAgentCount++
				}
			}
		}

		// Get gate requirements for the next transition from the new phase
		if nextPhase, err := phase.ChainNextPhase(phase.ResolveChain(run), result.ToPhase); err == nil {
			nextGateRequirements = phase.GateChecksForTransition(result.ToPhase, nextPhase)
		}
	}

	if flagJSON {
		out := map[string]interface{}{
			"from_phase":  result.FromPhase,
			"to_phase":    result.ToPhase,
			"event_type":  result.EventType,
			"gate_result": result.GateResult,
			"gate_tier":   result.GateTier,
			"advanced":    result.Advanced,
			"reason":      result.Reason,
		}
		if result.Advanced {
			out["active_agent_count"] = activeAgentCount
			if len(nextGateRequirements) > 0 {
				out["next_gate_requirements"] = nextGateRequirements
			}
		}
		if len(resolvedActions) > 0 {
			items := make([]map[string]interface{}, len(resolvedActions))
			for i, a := range resolvedActions {
				items[i] = actionToMap(a)
			}
			out["actions"] = items
		}
		json.NewEncoder(os.Stdout).Encode(out)
	} else {
		if result.Advanced {
			fmt.Printf("%s → %s\n", result.FromPhase, result.ToPhase)
			for _, a := range resolvedActions {
				argsStr := ""
				if a.Args != nil {
					argsStr = " " + *a.Args
				}
				fmt.Printf("  → %s%s  [%s]\n", a.Command, argsStr, a.Mode)
			}
		} else {
			fmt.Printf("%s (blocked: %s)\n", result.FromPhase, result.EventType)
		}
	}

	if result.Advanced {
		return 0
	}
	return 1
}

func cmdRunSkip(ctx context.Context, args []string) int {
	var reason, actor string
	var positional []string

	for i := 0; i < len(args); i++ {
		switch {
		case strings.HasPrefix(args[i], "--reason="):
			reason = strings.TrimPrefix(args[i], "--reason=")
		case strings.HasPrefix(args[i], "--actor="):
			actor = strings.TrimPrefix(args[i], "--actor=")
		default:
			positional = append(positional, args[i])
		}
	}

	if len(positional) < 2 {
		fmt.Fprintf(os.Stderr, "ic: run skip: usage: ic run skip <id> <phase> --reason=<text> [--actor=<name>]\n")
		return 3
	}
	runID := positional[0]
	targetPhase := positional[1]

	if reason == "" {
		slog.Error("run skip: --reason is required")
		return 3
	}

	d, err := openDB()
	if err != nil {
		slog.Error("run skip failed", "error", err)
		return 2
	}
	defer d.Close()

	store := phase.New(d.SqlDB())
	if err := store.SkipPhase(ctx, runID, targetPhase, reason, actor); err != nil {
		slog.Error("run skip failed", "error", err)
		return 1
	}

	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(map[string]string{
			"status": "skipped",
			"phase":  targetPhase,
		})
	} else {
		fmt.Printf("skipped: %s\n", targetPhase)
	}
	return 0
}

func cmdRunPhase(ctx context.Context, args []string) int {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "ic: run phase: usage: ic run phase <id>\n")
		return 3
	}

	d, err := openDB()
	if err != nil {
		slog.Error("run phase failed", "error", err)
		return 2
	}
	defer d.Close()

	store := phase.New(d.SqlDB())
	run, err := store.Get(ctx, args[0])
	if err != nil {
		if err == phase.ErrNotFound {
			slog.Error("run phase: not found", "id", args[0])
			return 1
		}
		slog.Error("run phase failed", "error", err)
		return 2
	}

	fmt.Println(run.Phase)
	return 0
}

func cmdRunList(ctx context.Context, args []string) int {
	activeOnly := false
	portfolioOnly := false
	var scopeFilter *string

	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--active":
			activeOnly = true
		case args[i] == "--portfolio":
			portfolioOnly = true
		case strings.HasPrefix(args[i], "--scope="):
			s := strings.TrimPrefix(args[i], "--scope=")
			scopeFilter = &s
		}
	}

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

func cmdRunEvents(ctx context.Context, args []string) int {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "ic: run events: usage: ic run events <id>\n")
		return 3
	}

	d, err := openDB()
	if err != nil {
		slog.Error("run events failed", "error", err)
		return 2
	}
	defer d.Close()

	store := phase.New(d.SqlDB())
	events, err := store.Events(ctx, args[0])
	if err != nil {
		slog.Error("run events failed", "error", err)
		return 2
	}

	if flagJSON {
		items := make([]map[string]interface{}, len(events))
		for i, e := range events {
			items[i] = eventToMap(e)
		}
		json.NewEncoder(os.Stdout).Encode(items)
	} else {
		for _, e := range events {
			gate := ""
			if e.GateResult != nil {
				gate = *e.GateResult
			}
			reason := ""
			if e.Reason != nil {
				reason = *e.Reason
			}
			fmt.Printf("%s → %s\t%s\t%s\t%s\n", e.FromPhase, e.ToPhase, e.EventType, gate, reason)
		}
	}
	return 0
}

func cmdRunCancel(ctx context.Context, args []string) int {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "ic: run cancel: usage: ic run cancel <id>\n")
		return 3
	}

	d, err := openDB()
	if err != nil {
		slog.Error("run cancel failed", "error", err)
		return 2
	}
	defer d.Close()

	store := phase.New(d.SqlDB())

	// Get current state for the event record
	run, err := store.Get(ctx, args[0])
	if err != nil {
		if err == phase.ErrNotFound {
			slog.Error("run cancel: not found", "id", args[0])
			return 1
		}
		slog.Error("run cancel failed", "error", err)
		return 2
	}

	if phase.IsTerminalStatus(run.Status) {
		slog.Error("run cancel: run already in terminal state", "status", run.Status)
		return 1
	}

	// Portfolio cascade: cancel portfolio + all active children atomically
	if run.ProjectDir == "" {
		if err := store.CancelPortfolio(ctx, args[0]); err != nil {
			slog.Error("run cancel failed", "error", err)
			return 2
		}
		// Record cancel events for portfolio and children
		store.AddEvent(ctx, &phase.PhaseEvent{
			RunID:     args[0],
			FromPhase: run.Phase,
			ToPhase:   run.Phase,
			EventType: phase.EventCancel,
		})
		children, err := store.GetChildren(ctx, args[0])
		if err != nil {
			slog.Warn("run cancel: could not list children", "error", err)
		}
		for _, c := range children {
			store.AddEvent(ctx, &phase.PhaseEvent{
				RunID:     c.ID,
				FromPhase: c.Phase,
				ToPhase:   c.Phase,
				EventType: phase.EventCancel,
				Reason:    strPtr("portfolio cancelled"),
			})
		}
		fmt.Printf("cancelled (portfolio + %d children)\n", len(children))
		return 0
	}

	if err := store.UpdateStatus(ctx, args[0], phase.StatusCancelled); err != nil {
		slog.Error("run cancel failed", "error", err)
		return 2
	}

	// Record cancel event
	store.AddEvent(ctx, &phase.PhaseEvent{
		RunID:     args[0],
		FromPhase: run.Phase,
		ToPhase:   run.Phase,
		EventType: phase.EventCancel,
	})

	fmt.Println("cancelled")
	return 0
}

func cmdRunRollback(ctx context.Context, args []string) int {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "ic: run rollback: usage: ic run rollback <id> --to-phase=<phase> [--reason=<text>] [--dry-run]\n")
		fmt.Fprintf(os.Stderr, "       ic run rollback <id> --layer=code [--phase=<phase>] [--format=json|text]\n")
		return 3
	}

	runID := args[0]
	var toPhase, reason, layer, filterPhase, format string
	dryRun := false

	for i := 1; i < len(args); i++ {
		switch {
		case strings.HasPrefix(args[i], "--to-phase="):
			toPhase = strings.TrimPrefix(args[i], "--to-phase=")
		case strings.HasPrefix(args[i], "--reason="):
			reason = strings.TrimPrefix(args[i], "--reason=")
		case strings.HasPrefix(args[i], "--layer="):
			layer = strings.TrimPrefix(args[i], "--layer=")
		case strings.HasPrefix(args[i], "--phase="):
			filterPhase = strings.TrimPrefix(args[i], "--phase=")
		case strings.HasPrefix(args[i], "--format="):
			format = strings.TrimPrefix(args[i], "--format=")
		case args[i] == "--dry-run":
			dryRun = true
		}
	}

	// Route: --layer=code → code rollback query
	if layer == "code" {
		return cmdRunRollbackCode(ctx, runID, filterPhase, format)
	}

	// Route: --to-phase → workflow rollback
	if toPhase == "" {
		slog.Error("run rollback: --to-phase or --layer required")
		return 3
	}

	return cmdRunRollbackWorkflow(ctx, runID, toPhase, reason, dryRun)
}

func cmdRunRollbackWorkflow(ctx context.Context, runID, toPhase, reason string, dryRun bool) int {
	d, err := openDB()
	if err != nil {
		slog.Error("run rollback failed", "error", err)
		return 2
	}
	defer d.Close()

	pStore := phase.New(d.SqlDB())
	rtStore := runtrack.New(d.SqlDB())
	dStore := dispatch.New(d.SqlDB(), nil)

	// Get current state for dry-run and validation
	run, err := pStore.Get(ctx, runID)
	if err != nil {
		if err == phase.ErrNotFound {
			slog.Error("run rollback: not found", "id", runID)
			return 1
		}
		slog.Error("run rollback failed", "error", err)
		return 2
	}

	chain := phase.ResolveChain(run)
	rolledBackPhases := phase.ChainPhasesBetween(chain, toPhase, run.Phase)
	if rolledBackPhases == nil {
		slog.Error("run rollback: target phase is not behind current phase", "target", toPhase, "current", run.Phase)
		return 1
	}

	if dryRun {
		output := map[string]interface{}{
			"dry_run":            true,
			"from_phase":         run.Phase,
			"to_phase":           toPhase,
			"rolled_back_phases": rolledBackPhases,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(output); err != nil {
			slog.Error("run rollback: output error failed", "error", err)
			return 2
		}
		return 0
	}

	// Set up event notifier for bus notifications
	notifier := event.NewNotifier()
	var rollbackEventLogger *slog.Logger
	if flagVerbose {
		rollbackEventLogger = slog.Default()
	}
	notifier.Subscribe("log", event.NewLogHandler(rollbackEventLogger))
	notifier.Subscribe("hook", event.NewHookHandler(run.ProjectDir, slog.Default()))

	callback := func(runID, eventType, fromPhase, toPhase, cbReason string) {
		notifier.Notify(ctx, event.Event{
			RunID:     runID,
			Source:    event.SourcePhase,
			Type:      eventType,
			FromState: fromPhase,
			ToState:   toPhase,
			Reason:    cbReason,
			Timestamp: time.Now(),
		})
	}

	// Perform workflow rollback
	result, err := phase.Rollback(ctx, pStore, runID, toPhase, reason, callback)
	if err != nil {
		slog.Error("run rollback failed", "error", err)
		if err == phase.ErrNotFound {
			return 1
		}
		return 2
	}

	// Cleanup: mark artifacts, cancel dispatches, fail agents — all in one transaction
	// to prevent partial state on SIGKILL between steps.
	cleanupErr := false
	markedArtifacts, err := rtStore.MarkArtifactsRolledBack(ctx, runID, result.RolledBackPhases)
	if err != nil {
		slog.Warn("run rollback: artifact marking failed", "error", err)
		cleanupErr = true
	}

	cancelledDispatches, err := dStore.CancelByRun(ctx, runID)
	if err != nil {
		slog.Warn("run rollback: dispatch cancellation failed", "error", err)
		cleanupErr = true
	}

	failedAgents, err := rtStore.FailAgentsByRun(ctx, runID)
	if err != nil {
		slog.Warn("run rollback: agent failure marking failed", "error", err)
		cleanupErr = true
	}

	// Output
	output := map[string]interface{}{
		"from_phase":           result.FromPhase,
		"to_phase":             result.ToPhase,
		"rolled_back_phases":   result.RolledBackPhases,
		"reason":               result.Reason,
		"cancelled_dispatches": cancelledDispatches,
		"marked_artifacts":     markedArtifacts,
		"failed_agents":        failedAgents,
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(output); err != nil {
		slog.Error("run rollback: output error failed", "error", err)
		return 2
	}

	// Return exit 2 if any cleanup step failed (C-03: don't silently return 0)
	if cleanupErr {
		return 2
	}
	return 0
}

func cmdRunRollbackCode(ctx context.Context, runID, filterPhase, format string) int {
	d, err := openDB()
	if err != nil {
		slog.Error("run rollback --layer=code failed", "error", err)
		return 2
	}
	defer d.Close()

	pStore := phase.New(d.SqlDB())
	rtStore := runtrack.New(d.SqlDB())

	// Verify run exists
	_, err = pStore.Get(ctx, runID)
	if err != nil {
		if err == phase.ErrNotFound {
			slog.Error("run rollback: not found", "id", runID)
			return 1
		}
		slog.Error("run rollback failed", "error", err)
		return 2
	}

	var phaseFilter *string
	if filterPhase != "" {
		phaseFilter = &filterPhase
	}

	entries, err := rtStore.ListArtifactsForCodeRollback(ctx, runID, phaseFilter)
	if err != nil {
		slog.Error("run rollback --layer=code failed", "error", err)
		return 2
	}

	if format == "text" {
		for _, e := range entries {
			dispatchName := "<unknown>"
			if e.DispatchName != nil {
				dispatchName = *e.DispatchName
			}
			hash := "<none>"
			if e.ContentHash != nil && len(*e.ContentHash) > 12 {
				hash = (*e.ContentHash)[:12] + "..."
			} else if e.ContentHash != nil {
				hash = *e.ContentHash
			}
			fmt.Printf("%-20s %-12s %-20s %-40s %s\n", e.Phase, e.Status, dispatchName, e.Path, hash)
		}
		return 0
	}

	// Default JSON output
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(entries); err != nil {
		slog.Error("run rollback --layer=code: output error failed", "error", err)
		return 2
	}
	return 0
}

func cmdRunSet(ctx context.Context, args []string) int {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "ic: run set: usage: ic run set <id> [--complexity=N] [--auto-advance=bool] [--force-full=bool] [--max-dispatches=N]\n")
		return 3
	}

	id := args[0]
	var complexity *int
	var autoAdvance *bool
	var forceFull *bool
	var maxDispatches *int

	for i := 1; i < len(args); i++ {
		switch {
		case strings.HasPrefix(args[i], "--complexity="):
			val := strings.TrimPrefix(args[i], "--complexity=")
			c, err := strconv.Atoi(val)
			if err != nil || c < 1 || c > 5 {
				slog.Error("run set: invalid complexity", "value", val)
				return 3
			}
			complexity = &c
		case strings.HasPrefix(args[i], "--auto-advance="):
			val := strings.TrimPrefix(args[i], "--auto-advance=")
			b, err := strconv.ParseBool(val)
			if err != nil {
				slog.Error("run set: invalid bool", "value", val)
				return 3
			}
			autoAdvance = &b
		case strings.HasPrefix(args[i], "--force-full="):
			val := strings.TrimPrefix(args[i], "--force-full=")
			b, err := strconv.ParseBool(val)
			if err != nil {
				slog.Error("run set: invalid bool", "value", val)
				return 3
			}
			forceFull = &b
		case strings.HasPrefix(args[i], "--max-dispatches="):
			val := strings.TrimPrefix(args[i], "--max-dispatches=")
			v, err := strconv.Atoi(val)
			if err != nil || v < 0 {
				slog.Error("run set: invalid max-dispatches", "value", val)
				return 3
			}
			maxDispatches = &v
		default:
			slog.Error("run set: unknown flag", "value", args[i])
			return 3
		}
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

func cmdRunCurrent(ctx context.Context, args []string) int {
	var project string

	for i := 0; i < len(args); i++ {
		switch {
		case strings.HasPrefix(args[i], "--project="):
			project = strings.TrimPrefix(args[i], "--project=")
		default:
			slog.Error("run current: unknown flag", "value", args[i])
			return 3
		}
	}

	if project == "" {
		cwd, err := os.Getwd()
		if err != nil {
			slog.Error("run current: cannot determine project dir", "error", err)
			return 2
		}
		project = cwd
	}

	d, err := openDB()
	if err != nil {
		slog.Error("run current failed", "error", err)
		return 2
	}
	defer d.Close()

	store := phase.New(d.SqlDB())
	run, err := store.Current(ctx, project)
	if err != nil {
		if err == phase.ErrNotFound {
			if flagJSON {
				json.NewEncoder(os.Stdout).Encode(map[string]interface{}{
					"found": false,
				})
			}
			return 1
		}
		slog.Error("run current failed", "error", err)
		return 2
	}

	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(map[string]interface{}{
			"found": true,
			"id":    run.ID,
			"phase": run.Phase,
			"goal":  run.Goal,
		})
	} else {
		fmt.Println(run.ID)
	}
	return 0
}

// --- Run Agent Commands ---

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
	var agentType, name, dispatchID string
	var positional []string

	for i := 0; i < len(args); i++ {
		switch {
		case strings.HasPrefix(args[i], "--type="):
			agentType = strings.TrimPrefix(args[i], "--type=")
		case strings.HasPrefix(args[i], "--name="):
			name = strings.TrimPrefix(args[i], "--name=")
		case strings.HasPrefix(args[i], "--dispatch-id="):
			dispatchID = strings.TrimPrefix(args[i], "--dispatch-id=")
		default:
			positional = append(positional, args[i])
		}
	}

	if len(positional) < 1 {
		fmt.Fprintf(os.Stderr, "ic: run agent add: usage: ic run agent add <run_id> --type=<type> [--name=<name>] [--dispatch-id=<id>]\n")
		return 3
	}
	runID := positional[0]

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
	var status string
	var positional []string

	for i := 0; i < len(args); i++ {
		switch {
		case strings.HasPrefix(args[i], "--status="):
			status = strings.TrimPrefix(args[i], "--status=")
		default:
			positional = append(positional, args[i])
		}
	}

	if len(positional) < 1 {
		fmt.Fprintf(os.Stderr, "ic: run agent update: usage: ic run agent update <agent_id> --status=<status>\n")
		return 3
	}
	agentID := positional[0]

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

// --- Run Artifact Commands ---

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

type replayGate struct {
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason,omitempty"`
}

type replayOutput struct {
	RunID     string            `json:"run_id"`
	Mode      string            `json:"mode"`
	RunStatus string            `json:"run_status"`
	Decisions []replay.Decision `json:"decisions"`
	Inputs    []*replay.Input   `json:"inputs"`
	Reexecute replayGate        `json:"reexecute"`
}

func cmdRunReplay(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "ic: run replay: usage: ic run replay <run_id> [--mode=simulate|reexecute] [--allow-live] [--limit=N]\n")
		fmt.Fprintf(os.Stderr, "                ic run replay inputs <run_id> [--limit=N]\n")
		fmt.Fprintf(os.Stderr, "                ic run replay record <run_id> --kind=<kind> [--key=<k>] [--payload=<json>] [--artifact-ref=<ref>] [--event-source=<src>] [--event-id=<id>]\n")
		return 3
	}

	switch args[0] {
	case "inputs":
		return cmdRunReplayInputs(ctx, args[1:])
	case "record":
		return cmdRunReplayRecord(ctx, args[1:])
	}

	runID := args[0]
	mode := "simulate"
	allowLive := false
	limit := 2000

	for _, arg := range args[1:] {
		switch {
		case strings.HasPrefix(arg, "--mode="):
			mode = strings.TrimPrefix(arg, "--mode=")
		case arg == "--allow-live":
			allowLive = true
		case strings.HasPrefix(arg, "--limit="):
			v, err := strconv.Atoi(strings.TrimPrefix(arg, "--limit="))
			if err != nil || v <= 0 {
				slog.Error("run replay: invalid --limit", "value", strings.TrimPrefix(arg, "--limit="))
				return 3
			}
			limit = v
		default:
			slog.Error("run replay: unknown flag", "value", arg)
			return 3
		}
	}

	if mode != "simulate" && mode != "reexecute" {
		slog.Error("run replay: invalid --mode", "value", mode)
		return 3
	}

	d, err := openDB()
	if err != nil {
		slog.Error("run replay failed", "error", err)
		return 2
	}
	defer d.Close()

	pStore := phase.New(d.SqlDB())
	run, err := pStore.Get(ctx, runID)
	if err != nil {
		if err == phase.ErrNotFound {
			slog.Error("run replay: run not found", "id", runID)
			return 1
		}
		slog.Error("run replay failed", "error", err)
		return 2
	}

	if run.Status != phase.StatusCompleted {
		slog.Error("run replay: run must be completed for deterministic replay", "status", run.Status)
		return 1
	}

	evStore := event.NewStore(d.SqlDB())
	events, err := evStore.ListEvents(ctx, runID, 0, 0, 0, 0, limit)
	if err != nil {
		slog.Error("run replay: list events failed", "error", err)
		return 2
	}

	replayStore := replay.New(d.SqlDB())
	inputs, err := replayStore.ListInputs(ctx, runID, limit*2)
	if err != nil {
		slog.Error("run replay: list inputs failed", "error", err)
		return 2
	}

	out := replayOutput{
		RunID:     runID,
		Mode:      mode,
		RunStatus: run.Status,
		Inputs:    inputs,
		Reexecute: replayGate{
			Allowed: false,
		},
	}
	out.Decisions = replay.BuildTimeline(events, inputs)

	if mode == "simulate" {
		out.Reexecute.Reason = "simulate mode has no side effects"
	} else {
		if !allowLive {
			out.Reexecute.Reason = "live reexecute is gated: pass --allow-live to request it"
		} else {
			out.Reexecute.Reason = "live reexecute is currently disallowed by kernel policy"
		}
	}

	if flagJSON {
		if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
			slog.Error("run replay: write failed", "error", err)
			return 2
		}
	} else {
		fmt.Printf("Run: %s (%s)\n", out.RunID, out.RunStatus)
		fmt.Printf("Mode: %s\n", out.Mode)
		fmt.Printf("Decisions: %d\n", len(out.Decisions))
		fmt.Printf("Recorded inputs: %d\n", len(out.Inputs))
		for _, d := range out.Decisions {
			fmt.Printf("  [%s#%d] %s %s -> %s\n", d.Source, d.EventID, d.Type, d.FromState, d.ToState)
		}
		fmt.Printf("Reexecute: allowed=%t (%s)\n", out.Reexecute.Allowed, out.Reexecute.Reason)
	}

	if mode == "reexecute" {
		return 1
	}
	return 0
}

func cmdRunReplayInputs(ctx context.Context, args []string) int {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "ic: run replay inputs: usage: ic run replay inputs <run_id> [--limit=N]\n")
		return 3
	}
	runID := args[0]
	limit := 1000
	for _, arg := range args[1:] {
		switch {
		case strings.HasPrefix(arg, "--limit="):
			v, err := strconv.Atoi(strings.TrimPrefix(arg, "--limit="))
			if err != nil || v <= 0 {
				slog.Error("run replay inputs: invalid --limit", "value", strings.TrimPrefix(arg, "--limit="))
				return 3
			}
			limit = v
		default:
			slog.Error("run replay inputs: unknown flag", "value", arg)
			return 3
		}
	}

	d, err := openDB()
	if err != nil {
		slog.Error("run replay inputs failed", "error", err)
		return 2
	}
	defer d.Close()

	replayStore := replay.New(d.SqlDB())
	inputs, err := replayStore.ListInputs(ctx, runID, limit)
	if err != nil {
		slog.Error("run replay inputs failed", "error", err)
		return 2
	}

	if flagJSON {
		if err := json.NewEncoder(os.Stdout).Encode(inputs); err != nil {
			slog.Error("run replay inputs: write failed", "error", err)
			return 2
		}
	} else {
		for _, in := range inputs {
			fmt.Printf("%d\t%s\t%s\t%s\n", in.ID, in.Kind, in.Key, in.Payload)
		}
	}
	return 0
}

func cmdRunReplayRecord(ctx context.Context, args []string) int {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "ic: run replay record: usage: ic run replay record <run_id> --kind=<kind> [--key=<k>] [--payload=<json>] [--artifact-ref=<ref>] [--event-source=<src>] [--event-id=<id>]\n")
		return 3
	}
	runID := args[0]
	var (
		kind, key, payload, artifactRef, eventSource string
		eventID                                      *int64
	)
	for _, arg := range args[1:] {
		switch {
		case strings.HasPrefix(arg, "--kind="):
			kind = strings.TrimPrefix(arg, "--kind=")
		case strings.HasPrefix(arg, "--key="):
			key = strings.TrimPrefix(arg, "--key=")
		case strings.HasPrefix(arg, "--payload="):
			payload = strings.TrimPrefix(arg, "--payload=")
		case strings.HasPrefix(arg, "--artifact-ref="):
			artifactRef = strings.TrimPrefix(arg, "--artifact-ref=")
		case strings.HasPrefix(arg, "--event-source="):
			eventSource = strings.TrimPrefix(arg, "--event-source=")
		case strings.HasPrefix(arg, "--event-id="):
			v, err := strconv.ParseInt(strings.TrimPrefix(arg, "--event-id="), 10, 64)
			if err != nil || v <= 0 {
				slog.Error("run replay record: invalid --event-id", "value", strings.TrimPrefix(arg, "--event-id="))
				return 3
			}
			eventID = &v
		default:
			slog.Error("run replay record: unknown flag", "value", arg)
			return 3
		}
	}
	if kind == "" {
		slog.Error("run replay record: --kind is required")
		return 3
	}
	if payload == "" {
		payload = "{}"
	}

	d, err := openDB()
	if err != nil {
		slog.Error("run replay record failed", "error", err)
		return 2
	}
	defer d.Close()

	pStore := phase.New(d.SqlDB())
	if _, err := pStore.Get(ctx, runID); err != nil {
		if err == phase.ErrNotFound {
			slog.Error("run replay record: run not found", "id", runID)
			return 1
		}
		slog.Error("run replay record failed", "error", err)
		return 2
	}

	replayStore := replay.New(d.SqlDB())
	in := &replay.Input{
		RunID:       runID,
		Kind:        kind,
		Key:         key,
		Payload:     payload,
		EventSource: eventSource,
		EventID:     eventID,
	}
	if artifactRef != "" {
		in.ArtifactRef = &artifactRef
	}
	id, err := replayStore.AddInput(ctx, in)
	if err != nil {
		slog.Error("run replay record failed", "error", err)
		return 2
	}
	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(map[string]interface{}{"id": id})
	} else {
		fmt.Printf("%d\n", id)
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
	var artifactPhase, path, artifactType, dispatch string
	var positional []string

	for i := 0; i < len(args); i++ {
		switch {
		case strings.HasPrefix(args[i], "--phase="):
			artifactPhase = strings.TrimPrefix(args[i], "--phase=")
		case strings.HasPrefix(args[i], "--path="):
			path = strings.TrimPrefix(args[i], "--path=")
		case strings.HasPrefix(args[i], "--type="):
			artifactType = strings.TrimPrefix(args[i], "--type=")
		case strings.HasPrefix(args[i], "--dispatch="):
			dispatch = strings.TrimPrefix(args[i], "--dispatch=")
		default:
			positional = append(positional, args[i])
		}
	}

	if len(positional) < 1 {
		fmt.Fprintf(os.Stderr, "ic: run artifact add: usage: ic run artifact add <run_id> --phase=<phase> --path=<path> [--type=<type>] [--dispatch=<id>]\n")
		return 3
	}
	runID := positional[0]

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
	var phaseFilter *string
	var positional []string

	for i := 0; i < len(args); i++ {
		switch {
		case strings.HasPrefix(args[i], "--phase="):
			s := strings.TrimPrefix(args[i], "--phase=")
			phaseFilter = &s
		default:
			positional = append(positional, args[i])
		}
	}

	if len(positional) < 1 {
		fmt.Fprintf(os.Stderr, "ic: run artifact list: usage: ic run artifact list <run_id> [--phase=<phase>]\n")
		return 3
	}
	runID := positional[0]

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
