package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/mistakeknot/intercore/internal/budget"
	"github.com/mistakeknot/intercore/internal/dispatch"
	"github.com/mistakeknot/intercore/internal/phase"
	"github.com/mistakeknot/intercore/internal/portfolio"
	"github.com/mistakeknot/intercore/internal/runtrack"
	"github.com/mistakeknot/intercore/internal/state"
)

func cmdGate(ctx context.Context, args []string) int {
	if len(args) == 0 {
		slog.Error("gate: missing subcommand", "expected", "check, override, rules")
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
		slog.Error("gate: unknown subcommand", "subcommand", args[0])
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
				slog.Error("gate check: invalid priority", "value", val)
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
		slog.Error("gate check failed", "error", err)
		return 2
	}
	defer d.Close()

	store := phase.New(d.SqlDB())
	rtStore := runtrack.New(d.SqlDB())
	dStore := dispatch.New(d.SqlDB(), nil)
	depStore := portfolio.NewDepStore(d.SqlDB())

	// Budget querier: check if run has budget enforcement
	var bq phase.BudgetQuerier
	run, runErr := store.Get(ctx, runID)
	if runErr == nil && run.BudgetEnforce {
		sStore := state.New(d.SqlDB())
		checker := budget.New(store, dStore, sStore, nil)
		bq = &cliBudgetQuerier{checker: checker}
	}

	// Resolve spec-defined gate rules from agency specs (if loaded).
	// Per-run gate rules (run.GateRules) take precedence — skip spec lookup when set.
	var specRules []phase.SpecGateRule
	if run != nil && run.GateRules == nil {
		sStore := state.New(d.SqlDB())
		gateKey := fmt.Sprintf("agency.gates.%s", run.Phase)
		gateJSON, gerr := sStore.Get(ctx, gateKey, runID)
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

	result, err := phase.EvaluateGate(ctx, store, runID, phase.GateConfig{
		Priority:  priority,
		SpecRules: specRules,
	}, rtStore, dStore, store, depStore, bq)
	if err != nil {
		if errors.Is(err, phase.ErrNotFound) {
			slog.Error("gate check: not found", "id", runID)
			return 1
		}
		if errors.Is(err, phase.ErrTerminalRun) || errors.Is(err, phase.ErrTerminalPhase) {
			slog.Error("gate check failed", "error", err)
			return 1
		}
		slog.Error("gate check failed", "error", err)
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
		slog.Error("gate override: --reason is required")
		return 3
	}

	d, err := openDB()
	if err != nil {
		slog.Error("gate override failed", "error", err)
		return 2
	}
	defer d.Close()

	store := phase.New(d.SqlDB())
	run, err := store.Get(ctx, runID)
	if err != nil {
		if err == phase.ErrNotFound {
			slog.Error("gate override: not found", "id", runID)
			return 1
		}
		slog.Error("gate override failed", "error", err)
		return 2
	}

	if phase.IsTerminalStatus(run.Status) {
		slog.Error("gate override: run is in terminal status", "status", run.Status)
		return 1
	}
	chain := phase.ResolveChain(run)
	if phase.ChainIsTerminal(chain, run.Phase) {
		slog.Error("gate override: run is already at terminal phase")
		return 1
	}

	fromPhase := run.Phase
	toPhase, err := phase.ChainNextPhase(chain, fromPhase)
	if err != nil {
		slog.Error("gate override failed", "error", err)
		return 2
	}

	// R3: UpdatePhase first, then record event.
	// If crash between, advance happened without audit — safer than audit without advance.
	if err := store.UpdatePhase(ctx, runID, fromPhase, toPhase); err != nil {
		slog.Error("gate override failed", "error", err)
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
		slog.Error("gate override: event failed", "error", err)
		// Phase already advanced — log but don't fail (R3 ordering)
	}

	// If we reached the terminal phase, mark the run as completed
	if phase.ChainIsTerminal(chain, toPhase) {
		if err := store.UpdateStatus(ctx, runID, phase.StatusCompleted); err != nil {
			slog.Error("gate override: status failed", "error", err)
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
	var phaseFilter, runID string
	for i := 0; i < len(args); i++ {
		switch {
		case strings.HasPrefix(args[i], "--phase="):
			phaseFilter = strings.TrimPrefix(args[i], "--phase=")
		case strings.HasPrefix(args[i], "--run="):
			runID = strings.TrimPrefix(args[i], "--run=")
		}
	}

	// If --run is specified, show per-run rules (or defaults if no custom gates)
	if runID != "" {
		d, err := openDB()
		if err != nil {
			slog.Error("gate rules failed", "error", err)
			return 2
		}
		defer d.Close()

		store := phase.New(d.SqlDB())
		run, err := store.Get(ctx, runID)
		if err != nil {
			slog.Error("gate rules failed", "error", err)
			return 1
		}

		if run.GateRules != nil {
			return printRunGateRules(run.GateRules, phaseFilter, "run")
		}
		// Fall through to show defaults
	}

	rules := phase.GateRulesInfo()
	source := "default"

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
				"source": source,
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

func printRunGateRules(rules map[string][]phase.SpecGateRule, phaseFilter, source string) int {
	if flagJSON {
		var out []map[string]interface{}
		for key, ruleList := range rules {
			parts := strings.SplitN(key, "→", 2)
			if len(parts) != 2 {
				continue
			}
			from, to := parts[0], parts[1]
			if phaseFilter != "" && from != phaseFilter {
				continue
			}
			checks := make([]map[string]string, len(ruleList))
			for i, r := range ruleList {
				checks[i] = map[string]string{"check": r.Check}
				if r.Phase != "" {
					checks[i]["phase"] = r.Phase
				}
				if r.Tier != "" {
					checks[i]["tier"] = r.Tier
				}
			}
			out = append(out, map[string]interface{}{
				"from":   from,
				"to":     to,
				"checks": checks,
				"source": source,
			})
		}
		json.NewEncoder(os.Stdout).Encode(out)
	} else {
		for key, ruleList := range rules {
			parts := strings.SplitN(key, "→", 2)
			if len(parts) != 2 {
				continue
			}
			from, to := parts[0], parts[1]
			if phaseFilter != "" && from != phaseFilter {
				continue
			}
			for _, r := range ruleList {
				phaseCol := ""
				if r.Phase != "" {
					phaseCol = " (phase: " + r.Phase + ")"
				}
				tierCol := ""
				if r.Tier != "" {
					tierCol = " [" + r.Tier + "]"
				}
				fmt.Printf("%s → %s\t%s%s%s\n", from, to, r.Check, phaseCol, tierCol)
			}
		}
	}
	return 0
}

func strPtr(s string) *string {
	return &s
}

// cliBudgetQuerier adapts budget.Checker to the phase.BudgetQuerier interface.
type cliBudgetQuerier struct {
	checker *budget.Checker
}

func (q *cliBudgetQuerier) IsBudgetExceeded(ctx context.Context, runID string) (bool, error) {
	result, err := q.checker.Check(ctx, runID)
	if err != nil {
		return false, err
	}
	return result.Exceeded, nil
}
