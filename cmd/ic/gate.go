package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/mistakeknot/interverse/infra/intercore/internal/dispatch"
	"github.com/mistakeknot/interverse/infra/intercore/internal/phase"
	"github.com/mistakeknot/interverse/infra/intercore/internal/runtrack"
)

func cmdGate(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "ic: gate: missing subcommand (check, override, rules)\n")
		return 3
	}

	switch args[0] {
	case "check":
		return cmdGateCheck(ctx, args[1:])
	case "override":
		return cmdGateOverride(ctx, args[1:])
	case "rules":
		return cmdGateRules(ctx, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "ic: gate: unknown subcommand: %s\n", args[0])
		return 3
	}
}

func cmdGateCheck(ctx context.Context, args []string) int {
	priority := 3
	var positional []string

	for i := 0; i < len(args); i++ {
		switch {
		case strings.HasPrefix(args[i], "--priority="):
			val := strings.TrimPrefix(args[i], "--priority=")
			p, err := strconv.Atoi(val)
			if err != nil {
				fmt.Fprintf(os.Stderr, "ic: gate check: invalid priority: %s\n", val)
				return 3
			}
			priority = p
		default:
			positional = append(positional, args[i])
		}
	}

	if len(positional) < 1 {
		fmt.Fprintf(os.Stderr, "ic: gate check: usage: ic gate check <run_id> [--priority=N]\n")
		return 3
	}
	runID := positional[0]

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: gate check: %v\n", err)
		return 2
	}
	defer d.Close()

	store := phase.New(d.SqlDB())
	rtStore := runtrack.New(d.SqlDB())
	dStore := dispatch.New(d.SqlDB(), nil)

	result, err := phase.EvaluateGate(ctx, store, runID, phase.GateConfig{
		Priority: priority,
	}, rtStore, dStore, store)
	if err != nil {
		if errors.Is(err, phase.ErrNotFound) {
			fmt.Fprintf(os.Stderr, "ic: gate check: not found: %s\n", runID)
			return 1
		}
		if errors.Is(err, phase.ErrTerminalRun) || errors.Is(err, phase.ErrTerminalPhase) {
			fmt.Fprintf(os.Stderr, "ic: gate check: %v\n", err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "ic: gate check: %v\n", err)
		return 2
	}

	if flagJSON {
		out := map[string]interface{}{
			"run_id":     result.RunID,
			"from_phase": result.FromPhase,
			"to_phase":   result.ToPhase,
			"result":     result.Result,
			"tier":       result.Tier,
		}
		if result.Evidence != nil {
			out["evidence"] = result.Evidence
		}
		json.NewEncoder(os.Stdout).Encode(out)
	} else {
		fmt.Printf("%s → %s: %s (tier: %s)\n", result.FromPhase, result.ToPhase, result.Result, result.Tier)
		if result.Evidence != nil {
			for _, c := range result.Evidence.Conditions {
				indicator := "PASS"
				if c.Result == phase.GateFail {
					indicator = "FAIL"
				}
				detail := ""
				if c.Detail != "" {
					detail = " — " + c.Detail
				}
				if c.Count != nil {
					fmt.Printf("  [%s] %s (count: %d)%s\n", indicator, c.Check, *c.Count, detail)
				} else {
					fmt.Printf("  [%s] %s%s\n", indicator, c.Check, detail)
				}
			}
		}
	}

	if result.Result == phase.GatePass || result.Result == phase.GateNone {
		return 0
	}
	return 1
}

func cmdGateOverride(ctx context.Context, args []string) int {
	var reason string
	var positional []string

	for i := 0; i < len(args); i++ {
		switch {
		case strings.HasPrefix(args[i], "--reason="):
			reason = strings.TrimPrefix(args[i], "--reason=")
		default:
			positional = append(positional, args[i])
		}
	}

	if len(positional) < 1 {
		fmt.Fprintf(os.Stderr, "ic: gate override: usage: ic gate override <run_id> --reason=<reason>\n")
		return 3
	}
	runID := positional[0]

	if reason == "" {
		fmt.Fprintf(os.Stderr, "ic: gate override: --reason is required\n")
		return 3
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: gate override: %v\n", err)
		return 2
	}
	defer d.Close()

	store := phase.New(d.SqlDB())
	run, err := store.Get(ctx, runID)
	if err != nil {
		if err == phase.ErrNotFound {
			fmt.Fprintf(os.Stderr, "ic: gate override: not found: %s\n", runID)
			return 1
		}
		fmt.Fprintf(os.Stderr, "ic: gate override: %v\n", err)
		return 2
	}

	if phase.IsTerminalStatus(run.Status) {
		fmt.Fprintf(os.Stderr, "ic: gate override: run is %s\n", run.Status)
		return 1
	}
	chain := phase.ResolveChain(run)
	if phase.ChainIsTerminal(chain, run.Phase) {
		fmt.Fprintf(os.Stderr, "ic: gate override: run is already at terminal phase\n")
		return 1
	}

	fromPhase := run.Phase
	toPhase, err := phase.ChainNextPhase(chain, fromPhase)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: gate override: %v\n", err)
		return 2
	}

	// R3: UpdatePhase first, then record event.
	// If crash between, advance happened without audit — safer than audit without advance.
	if err := store.UpdatePhase(ctx, runID, fromPhase, toPhase); err != nil {
		fmt.Fprintf(os.Stderr, "ic: gate override: %v\n", err)
		return 2
	}

	if err := store.AddEvent(ctx, &phase.PhaseEvent{
		RunID:      runID,
		FromPhase:  fromPhase,
		ToPhase:    toPhase,
		EventType:  phase.EventOverride,
		GateResult: strPtr(phase.GateFail),
		GateTier:   strPtr(phase.TierHard),
		Reason:     strPtr(reason),
	}); err != nil {
		fmt.Fprintf(os.Stderr, "ic: gate override: event: %v\n", err)
		// Phase already advanced — log but don't fail (R3 ordering)
	}

	// If we reached the terminal phase, mark the run as completed
	if phase.ChainIsTerminal(chain, toPhase) {
		if err := store.UpdateStatus(ctx, runID, phase.StatusCompleted); err != nil {
			fmt.Fprintf(os.Stderr, "ic: gate override: status: %v\n", err)
		}
	}

	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(map[string]interface{}{
			"from_phase": fromPhase,
			"to_phase":   toPhase,
			"reason":     reason,
		})
	} else {
		fmt.Printf("%s → %s (override: %s)\n", fromPhase, toPhase, reason)
	}
	return 0
}

func cmdGateRules(ctx context.Context, args []string) int {
	var phaseFilter string
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "--phase=") {
			phaseFilter = strings.TrimPrefix(args[i], "--phase=")
		}
	}

	rules := phase.GateRulesInfo()

	if flagJSON {
		var out []map[string]interface{}
		for _, r := range rules {
			if phaseFilter != "" && r.From != phaseFilter {
				continue
			}
			checks := make([]map[string]string, len(r.Checks))
			for i, c := range r.Checks {
				checks[i] = map[string]string{"check": c.Check}
				if c.Phase != "" {
					checks[i]["phase"] = c.Phase
				}
			}
			out = append(out, map[string]interface{}{
				"from":   r.From,
				"to":     r.To,
				"checks": checks,
			})
		}
		json.NewEncoder(os.Stdout).Encode(out)
	} else {
		for _, r := range rules {
			if phaseFilter != "" && r.From != phaseFilter {
				continue
			}
			for _, c := range r.Checks {
				phaseCol := ""
				if c.Phase != "" {
					phaseCol = " (phase: " + c.Phase + ")"
				}
				fmt.Printf("%s → %s\t%s%s\n", r.From, r.To, c.Check, phaseCol)
			}
		}
	}
	return 0
}

func strPtr(s string) *string {
	return &s
}
