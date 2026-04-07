package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/mistakeknot/intercore/internal/action"
	"github.com/mistakeknot/intercore/internal/budget"
	"github.com/mistakeknot/intercore/internal/cli"
	"github.com/mistakeknot/intercore/internal/dispatch"
	"github.com/mistakeknot/intercore/internal/event"
	"github.com/mistakeknot/intercore/internal/phase"
	portfoliopkg "github.com/mistakeknot/intercore/internal/portfolio"
	"github.com/mistakeknot/intercore/internal/runtrack"
	"github.com/mistakeknot/intercore/internal/state"
)

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
	f := cli.ParseFlags(args)

	disableGates := f.Bool("disable-gates")
	skipReason := f.String("skip-reason", "")
	calibrationFile := f.String("calibration-file", "")

	priority, err := f.Int("priority", 1) // TierHard: evaluate AND block on gate failure
	if err != nil {
		slog.Error("run advance: invalid priority", "value", f.String("priority", ""))
		return 3
	}

	if len(f.Positionals) < 1 {
		fmt.Fprintf(os.Stderr, "ic: run advance: usage: ic run advance <id> [--priority=N] [--disable-gates] [--skip-reason=S]\n")
		return 3
	}
	id := f.Positionals[0]

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
	f := cli.ParseFlags(args)
	reason := f.String("reason", "")
	actor := f.String("actor", "")

	if len(f.Positionals) < 2 {
		fmt.Fprintf(os.Stderr, "ic: run skip: usage: ic run skip <id> <phase> --reason=<text> [--actor=<name>]\n")
		return 3
	}
	runID := f.Positionals[0]
	targetPhase := f.Positionals[1]

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
	f := cli.ParseFlags(args)

	if len(f.Positionals) < 1 {
		fmt.Fprintf(os.Stderr, "ic: run rollback: usage: ic run rollback <id> --to-phase=<phase> [--reason=<text>] [--dry-run]\n")
		fmt.Fprintf(os.Stderr, "       ic run rollback <id> --layer=code [--phase=<phase>] [--format=json|text]\n")
		return 3
	}

	runID := f.Positionals[0]
	toPhase := f.String("to-phase", "")
	reason := f.String("reason", "")
	layer := f.String("layer", "")
	filterPhase := f.String("phase", "")
	format := f.String("format", "")
	dryRun := f.Bool("dry-run")

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
func cmdRunCurrent(ctx context.Context, args []string) int {
	f := cli.ParseFlags(args)
	project := f.String("project", "")

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
