package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mistakeknot/interverse/infra/intercore/internal/budget"
	"github.com/mistakeknot/interverse/infra/intercore/internal/dispatch"
	"github.com/mistakeknot/interverse/infra/intercore/internal/event"
	"github.com/mistakeknot/interverse/infra/intercore/internal/phase"
	"github.com/mistakeknot/interverse/infra/intercore/internal/runtrack"
	"github.com/mistakeknot/interverse/infra/intercore/internal/state"
)

// --- Run Commands ---

func cmdRun(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "ic: run: missing subcommand (create, status, advance, skip, rollback, phase, list, events, cancel, set, current, agent, artifact, tokens, budget)\n")
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
	case "tokens":
		return cmdRunTokens(ctx, args[1:])
	case "budget":
		return cmdRunBudget(ctx, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "ic: run: unknown subcommand: %s\n", args[0])
		return 3
	}
}

func cmdRunCreate(ctx context.Context, args []string) int {
	var project, goal, scopeID, phasesJSON string
	complexity := 3
	var tokenBudget int64
	budgetWarnPct := 80

	for i := 0; i < len(args); i++ {
		switch {
		case strings.HasPrefix(args[i], "--project="):
			project = strings.TrimPrefix(args[i], "--project=")
		case strings.HasPrefix(args[i], "--goal="):
			goal = strings.TrimPrefix(args[i], "--goal=")
		case strings.HasPrefix(args[i], "--complexity="):
			val := strings.TrimPrefix(args[i], "--complexity=")
			c, err := strconv.Atoi(val)
			if err != nil || c < 1 || c > 5 {
				fmt.Fprintf(os.Stderr, "ic: run create: invalid complexity (1-5): %s\n", val)
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
				fmt.Fprintf(os.Stderr, "ic: run create: invalid token-budget (positive integer): %s\n", val)
				return 3
			}
			tokenBudget = v
		case strings.HasPrefix(args[i], "--budget-warn-pct="):
			val := strings.TrimPrefix(args[i], "--budget-warn-pct=")
			v, err := strconv.Atoi(val)
			if err != nil || v < 1 || v > 99 {
				fmt.Fprintf(os.Stderr, "ic: run create: invalid budget-warn-pct (1-99): %s\n", val)
				return 3
			}
			budgetWarnPct = v
		default:
			fmt.Fprintf(os.Stderr, "ic: run create: unknown flag: %s\n", args[i])
			return 3
		}
	}

	if project == "" {
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(os.Stderr, "ic: run create: cannot determine project dir: %v\n", err)
			return 2
		}
		project = cwd
	}

	if goal == "" {
		fmt.Fprintf(os.Stderr, "ic: run create: --goal is required\n")
		return 3
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: run create: %v\n", err)
		return 2
	}
	defer d.Close()

	// Validate custom phases if provided
	var customPhases []string
	if phasesJSON != "" {
		parsed, err := phase.ParsePhaseChain(phasesJSON)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ic: run create: %v\n", err)
			return 3
		}
		customPhases = parsed
	}

	run := &phase.Run{
		ProjectDir:    project,
		Goal:          goal,
		Complexity:    complexity,
		AutoAdvance:   true,
		BudgetWarnPct: budgetWarnPct,
		Phases:        customPhases,
	}
	if tokenBudget > 0 {
		run.TokenBudget = &tokenBudget
	}
	if scopeID != "" {
		run.ScopeID = &scopeID
	}

	store := phase.New(d.SqlDB())
	id, err := store.Create(ctx, run)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: run create: %v\n", err)
		return 2
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
		fmt.Fprintf(os.Stderr, "ic: run status: %v\n", err)
		return 2
	}
	defer d.Close()

	store := phase.New(d.SqlDB())
	run, err := store.Get(ctx, args[0])
	if err != nil {
		if err == phase.ErrNotFound {
			fmt.Fprintf(os.Stderr, "ic: run status: not found: %s\n", args[0])
			return 1
		}
		fmt.Fprintf(os.Stderr, "ic: run status: %v\n", err)
		return 2
	}

	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(runToMap(run))
	} else {
		printRun(run)
	}
	return 0
}

func cmdRunAdvance(ctx context.Context, args []string) int {
	var id string
	priority := 4
	disableGates := false
	skipReason := ""

	var positional []string
	for i := 0; i < len(args); i++ {
		switch {
		case strings.HasPrefix(args[i], "--priority="):
			val := strings.TrimPrefix(args[i], "--priority=")
			p, err := strconv.Atoi(val)
			if err != nil {
				fmt.Fprintf(os.Stderr, "ic: run advance: invalid priority: %s\n", val)
				return 3
			}
			priority = p
		case args[i] == "--disable-gates":
			disableGates = true
		case strings.HasPrefix(args[i], "--skip-reason="):
			skipReason = strings.TrimPrefix(args[i], "--skip-reason=")
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
		fmt.Fprintf(os.Stderr, "ic: run advance: %v\n", err)
		return 2
	}
	defer d.Close()

	store := phase.New(d.SqlDB())
	rtStore := runtrack.New(d.SqlDB())
	evStore := event.NewStore(d.SqlDB())

	// Set up event notifier with handlers
	notifier := event.NewNotifier()
	notifier.Subscribe("log", event.NewLogHandler(os.Stderr, !flagVerbose))

	// Get run info for project dir (needed by hook handler)
	run, err := store.Get(ctx, id)
	if err != nil {
		if err == phase.ErrNotFound {
			fmt.Fprintf(os.Stderr, "ic: run advance: not found: %s\n", id)
			return 1
		}
		fmt.Fprintf(os.Stderr, "ic: run advance: %v\n", err)
		return 2
	}
	notifier.Subscribe("hook", event.NewHookHandler(run.ProjectDir, os.Stderr))

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
		if err := evStore.AddDispatchEvent(ctx, dispatchID, runID, fromStatus, toStatus, "status_change", ""); err != nil {
			fmt.Fprintf(os.Stderr, "[event] dispatch event: %v\n", err)
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
	notifier.Subscribe("spawn", event.NewSpawnHandler(rtStore, spawner, os.Stderr))

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

	result, err := phase.Advance(ctx, store, id, phase.GateConfig{
		Priority:   priority,
		DisableAll: disableGates,
		SkipReason: skipReason,
	}, rtStore, dStore, phaseCallback)
	if err != nil {
		if err == phase.ErrNotFound {
			fmt.Fprintf(os.Stderr, "ic: run advance: not found: %s\n", id)
			return 1
		}
		if err == phase.ErrTerminalRun || err == phase.ErrTerminalPhase {
			fmt.Fprintf(os.Stderr, "ic: run advance: %v\n", err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "ic: run advance: %v\n", err)
		return 2
	}

	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(map[string]interface{}{
			"from_phase":  result.FromPhase,
			"to_phase":    result.ToPhase,
			"event_type":  result.EventType,
			"gate_result": result.GateResult,
			"gate_tier":   result.GateTier,
			"advanced":    result.Advanced,
			"reason":      result.Reason,
		})
	} else {
		if result.Advanced {
			fmt.Printf("%s → %s\n", result.FromPhase, result.ToPhase)
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
		fmt.Fprintf(os.Stderr, "ic: run skip: --reason is required\n")
		return 3
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: run skip: %v\n", err)
		return 2
	}
	defer d.Close()

	store := phase.New(d.SqlDB())
	if err := store.SkipPhase(ctx, runID, targetPhase, reason, actor); err != nil {
		fmt.Fprintf(os.Stderr, "ic: run skip: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "ic: run phase: %v\n", err)
		return 2
	}
	defer d.Close()

	store := phase.New(d.SqlDB())
	run, err := store.Get(ctx, args[0])
	if err != nil {
		if err == phase.ErrNotFound {
			fmt.Fprintf(os.Stderr, "ic: run phase: not found: %s\n", args[0])
			return 1
		}
		fmt.Fprintf(os.Stderr, "ic: run phase: %v\n", err)
		return 2
	}

	fmt.Println(run.Phase)
	return 0
}

func cmdRunList(ctx context.Context, args []string) int {
	activeOnly := false
	var scopeFilter *string

	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--active":
			activeOnly = true
		case strings.HasPrefix(args[i], "--scope="):
			s := strings.TrimPrefix(args[i], "--scope=")
			scopeFilter = &s
		}
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: run list: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "ic: run list: %v\n", err)
		return 2
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
		fmt.Fprintf(os.Stderr, "ic: run events: %v\n", err)
		return 2
	}
	defer d.Close()

	store := phase.New(d.SqlDB())
	events, err := store.Events(ctx, args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: run events: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "ic: run cancel: %v\n", err)
		return 2
	}
	defer d.Close()

	store := phase.New(d.SqlDB())

	// Get current state for the event record
	run, err := store.Get(ctx, args[0])
	if err != nil {
		if err == phase.ErrNotFound {
			fmt.Fprintf(os.Stderr, "ic: run cancel: not found: %s\n", args[0])
			return 1
		}
		fmt.Fprintf(os.Stderr, "ic: run cancel: %v\n", err)
		return 2
	}

	if phase.IsTerminalStatus(run.Status) {
		fmt.Fprintf(os.Stderr, "ic: run cancel: run already %s\n", run.Status)
		return 1
	}

	if err := store.UpdateStatus(ctx, args[0], phase.StatusCancelled); err != nil {
		fmt.Fprintf(os.Stderr, "ic: run cancel: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "ic: run rollback: --to-phase or --layer required\n")
		return 3
	}

	return cmdRunRollbackWorkflow(ctx, runID, toPhase, reason, dryRun)
}

func cmdRunRollbackWorkflow(ctx context.Context, runID, toPhase, reason string, dryRun bool) int {
	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: run rollback: %v\n", err)
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
			fmt.Fprintf(os.Stderr, "ic: run rollback: not found: %s\n", runID)
			return 1
		}
		fmt.Fprintf(os.Stderr, "ic: run rollback: %v\n", err)
		return 2
	}

	chain := phase.ResolveChain(run)
	rolledBackPhases := phase.ChainPhasesBetween(chain, toPhase, run.Phase)
	if rolledBackPhases == nil {
		fmt.Fprintf(os.Stderr, "ic: run rollback: target phase %q is not behind current phase %q\n", toPhase, run.Phase)
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
			fmt.Fprintf(os.Stderr, "ic: run rollback: output error: %v\n", err)
			return 2
		}
		return 0
	}

	// Set up event notifier for bus notifications
	notifier := event.NewNotifier()
	notifier.Subscribe("log", event.NewLogHandler(os.Stderr, !flagVerbose))
	notifier.Subscribe("hook", event.NewHookHandler(run.ProjectDir, os.Stderr))

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
		fmt.Fprintf(os.Stderr, "ic: run rollback: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "ic: run rollback: warning: artifact marking failed: %v\n", err)
		cleanupErr = true
	}

	cancelledDispatches, err := dStore.CancelByRun(ctx, runID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: run rollback: warning: dispatch cancellation failed: %v\n", err)
		cleanupErr = true
	}

	failedAgents, err := rtStore.FailAgentsByRun(ctx, runID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: run rollback: warning: agent failure marking failed: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "ic: run rollback: output error: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "ic: run rollback --layer=code: %v\n", err)
		return 2
	}
	defer d.Close()

	pStore := phase.New(d.SqlDB())
	rtStore := runtrack.New(d.SqlDB())

	// Verify run exists
	_, err = pStore.Get(ctx, runID)
	if err != nil {
		if err == phase.ErrNotFound {
			fmt.Fprintf(os.Stderr, "ic: run rollback: not found: %s\n", runID)
			return 1
		}
		fmt.Fprintf(os.Stderr, "ic: run rollback: %v\n", err)
		return 2
	}

	var phaseFilter *string
	if filterPhase != "" {
		phaseFilter = &filterPhase
	}

	entries, err := rtStore.ListArtifactsForCodeRollback(ctx, runID, phaseFilter)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: run rollback --layer=code: %v\n", err)
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
			fmt.Printf("%-20s %-20s %-40s %s\n", e.Phase, dispatchName, e.Path, hash)
		}
		return 0
	}

	// Default JSON output
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(entries); err != nil {
		fmt.Fprintf(os.Stderr, "ic: run rollback --layer=code: output error: %v\n", err)
		return 2
	}
	return 0
}

func cmdRunSet(ctx context.Context, args []string) int {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "ic: run set: usage: ic run set <id> [--complexity=N] [--auto-advance=bool] [--force-full=bool]\n")
		return 3
	}

	id := args[0]
	var complexity *int
	var autoAdvance *bool
	var forceFull *bool

	for i := 1; i < len(args); i++ {
		switch {
		case strings.HasPrefix(args[i], "--complexity="):
			val := strings.TrimPrefix(args[i], "--complexity=")
			c, err := strconv.Atoi(val)
			if err != nil || c < 1 || c > 5 {
				fmt.Fprintf(os.Stderr, "ic: run set: invalid complexity (1-5): %s\n", val)
				return 3
			}
			complexity = &c
		case strings.HasPrefix(args[i], "--auto-advance="):
			val := strings.TrimPrefix(args[i], "--auto-advance=")
			b, err := strconv.ParseBool(val)
			if err != nil {
				fmt.Fprintf(os.Stderr, "ic: run set: invalid bool: %s\n", val)
				return 3
			}
			autoAdvance = &b
		case strings.HasPrefix(args[i], "--force-full="):
			val := strings.TrimPrefix(args[i], "--force-full=")
			b, err := strconv.ParseBool(val)
			if err != nil {
				fmt.Fprintf(os.Stderr, "ic: run set: invalid bool: %s\n", val)
				return 3
			}
			forceFull = &b
		default:
			fmt.Fprintf(os.Stderr, "ic: run set: unknown flag: %s\n", args[i])
			return 3
		}
	}

	if complexity == nil && autoAdvance == nil && forceFull == nil {
		fmt.Fprintf(os.Stderr, "ic: run set: no settings to update\n")
		return 3
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: run set: %v\n", err)
		return 2
	}
	defer d.Close()

	store := phase.New(d.SqlDB())

	// Get current state for the event record
	run, err := store.Get(ctx, id)
	if err != nil {
		if err == phase.ErrNotFound {
			fmt.Fprintf(os.Stderr, "ic: run set: not found: %s\n", id)
			return 1
		}
		fmt.Fprintf(os.Stderr, "ic: run set: %v\n", err)
		return 2
	}

	if err := store.UpdateSettings(ctx, id, complexity, autoAdvance, forceFull); err != nil {
		fmt.Fprintf(os.Stderr, "ic: run set: %v\n", err)
		return 2
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
			fmt.Fprintf(os.Stderr, "ic: run current: unknown flag: %s\n", args[i])
			return 3
		}
	}

	if project == "" {
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(os.Stderr, "ic: run current: cannot determine project dir: %v\n", err)
			return 2
		}
		project = cwd
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: run current: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "ic: run current: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "ic: run agent: missing subcommand (add, list, update)\n")
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
		fmt.Fprintf(os.Stderr, "ic: run agent: unknown subcommand: %s\n", args[0])
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
		fmt.Fprintf(os.Stderr, "ic: run agent add: %v\n", err)
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
			fmt.Fprintf(os.Stderr, "ic: run agent add: run not found: %s\n", runID)
			return 1
		}
		fmt.Fprintf(os.Stderr, "ic: run agent add: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "ic: run agent list: %v\n", err)
		return 2
	}
	defer d.Close()

	store := runtrack.New(d.SqlDB())
	agents, err := store.ListAgents(ctx, runID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: run agent list: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "ic: run agent update: --status is required\n")
		return 3
	}

	switch status {
	case runtrack.StatusActive, runtrack.StatusCompleted, runtrack.StatusFailed:
		// valid
	default:
		fmt.Fprintf(os.Stderr, "ic: run agent update: invalid status %q (must be active, completed, or failed)\n", status)
		return 3
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: run agent update: %v\n", err)
		return 2
	}
	defer d.Close()

	store := runtrack.New(d.SqlDB())
	if err := store.UpdateAgent(ctx, agentID, status); err != nil {
		if err == runtrack.ErrAgentNotFound {
			fmt.Fprintf(os.Stderr, "ic: run agent update: not found: %s\n", agentID)
			return 1
		}
		fmt.Fprintf(os.Stderr, "ic: run agent update: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "ic: run tokens: %v\n", err)
		return 2
	}
	defer d.Close()

	dStore := dispatch.New(d.SqlDB(), nil)
	agg, err := dStore.AggregateTokens(ctx, runID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: run tokens: %v\n", err)
		return 2
	}

	total := agg.TotalIn + agg.TotalOut
	var cacheRatio float64
	if agg.TotalIn > 0 {
		cacheRatio = float64(agg.TotalCache) / float64(agg.TotalIn+agg.TotalCache) * 100
	}

	if flagJSON {
		out := map[string]interface{}{
			"run_id":       runID,
			"input_tokens": agg.TotalIn,
			"output_tokens": agg.TotalOut,
			"cache_hits":   agg.TotalCache,
			"total_tokens": total,
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
		fmt.Fprintf(os.Stderr, "ic: run budget: %v\n", err)
		return 2
	}
	defer d.Close()

	pStore := phase.New(d.SqlDB())
	dStore := dispatch.New(d.SqlDB(), nil)
	sStore := state.New(d.SqlDB())

	checker := budget.New(pStore, dStore, sStore, nil)
	result, err := checker.Check(ctx, runID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: run budget: %v\n", err)
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
			"run_id":      result.RunID,
			"budget":      result.Budget,
			"used":        result.Used,
			"warn_pct":    result.WarnPct,
			"usage_pct":   pct,
			"warning":     result.Warning,
			"exceeded":    result.Exceeded,
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
		fmt.Fprintf(os.Stderr, "ic: run artifact: missing subcommand (add, list)\n")
		return 3
	}

	switch args[0] {
	case "add":
		return cmdRunArtifactAdd(ctx, args[1:])
	case "list":
		return cmdRunArtifactList(ctx, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "ic: run artifact: unknown subcommand: %s\n", args[0])
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
		fmt.Fprintf(os.Stderr, "ic: run artifact add: --phase is required\n")
		return 3
	}
	if path == "" {
		fmt.Fprintf(os.Stderr, "ic: run artifact add: --path is required\n")
		return 3
	}
	if artifactType == "" {
		artifactType = "file"
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: run artifact add: %v\n", err)
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
			fmt.Fprintf(os.Stderr, "ic: run artifact add: run not found: %s\n", runID)
			return 1
		}
		fmt.Fprintf(os.Stderr, "ic: run artifact add: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "ic: run artifact list: %v\n", err)
		return 2
	}
	defer d.Close()

	store := runtrack.New(d.SqlDB())
	artifacts, err := store.ListArtifacts(ctx, runID, phaseFilter)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: run artifact list: %v\n", err)
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
}
