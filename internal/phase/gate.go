package phase

import (
	"context"
	"encoding/json"
	"fmt"
)

// EventOverride is the event type for manual gate overrides.
const EventOverride = "override"

// Check type constants for gateRules.
const (
	CheckArtifactExists   = "artifact_exists"
	CheckAgentsComplete   = "agents_complete"
	CheckVerdictExists    = "verdict_exists"
	CheckChildrenAtPhase  = "children_at_phase"
)

// RuntrackQuerier abstracts runtrack.Store queries needed by gate evaluation.
// Implemented by runtrack.Store; tests can use stubs.
type RuntrackQuerier interface {
	CountArtifacts(ctx context.Context, runID, phase string) (int, error)
	CountActiveAgents(ctx context.Context, runID string) (int, error)
}

// VerdictQuerier abstracts dispatch.Store queries needed by gate evaluation.
// Implemented by dispatch.Store; tests can use stubs.
type VerdictQuerier interface {
	HasVerdict(ctx context.Context, scopeID string) (bool, error)
}

// PortfolioQuerier abstracts queries for portfolio run children.
// Implemented by phase.Store; tests can use stubs.
type PortfolioQuerier interface {
	GetChildren(ctx context.Context, runID string) ([]*Run, error)
}

// GateCondition represents a single check within a gate evaluation.
type GateCondition struct {
	Check  string `json:"check"`
	Phase  string `json:"phase,omitempty"`
	Result string `json:"result"`
	Count  *int   `json:"count,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// GateEvidence is the structured result of a gate evaluation.
type GateEvidence struct {
	Conditions []GateCondition `json:"conditions"`
}

// String serializes evidence to JSON for storage in phase_events.reason.
func (e *GateEvidence) String() string {
	b, err := json.Marshal(e)
	if err != nil {
		return fmt.Sprintf(`{"error":%q}`, err.Error())
	}
	return string(b)
}

// GateCheckResult is the public result of a dry-run gate evaluation.
type GateCheckResult struct {
	RunID     string
	FromPhase string
	ToPhase   string
	Result    string
	Tier      string
	Evidence  *GateEvidence
}

// gateRule defines a single gate check to perform for a phase transition.
type gateRule struct {
	check string // CheckArtifactExists, CheckAgentsComplete, CheckVerdictExists
	phase string // which phase's artifacts to check (empty = not applicable)
}

// gateRules maps (from, to) phase pairs to their required checks.
// Transitions not in this table have no gate requirements.
// Skip-transitions (complexity-based phase skipping) bypass this table entirely.
var gateRules = map[[2]string][]gateRule{
	{PhaseBrainstorm, PhaseBrainstormReviewed}: {
		{check: CheckArtifactExists, phase: PhaseBrainstorm},
	},
	{PhaseBrainstormReviewed, PhaseStrategized}: {
		{check: CheckArtifactExists, phase: PhaseBrainstormReviewed},
	},
	{PhaseStrategized, PhasePlanned}: {
		{check: CheckArtifactExists, phase: PhaseStrategized},
	},
	{PhasePlanned, PhaseExecuting}: {
		{check: CheckArtifactExists, phase: PhasePlanned},
	},
	{PhaseExecuting, PhaseReview}: {
		{check: CheckAgentsComplete},
	},
	{PhaseReview, PhasePolish}: {
		{check: CheckVerdictExists},
	},
	// polish → reflect: no gate requirements (pass-through)
	// reflect → done: soft gate — requires reflect artifact
	{PhaseReflect, PhaseDone}: {
		{check: CheckArtifactExists, phase: PhaseReflect},
	},
}

// evaluateGate checks whether a phase transition should be allowed.
// Returns gate result, tier, structured evidence, and any error.
func evaluateGate(ctx context.Context, run *Run, cfg GateConfig, from, to string, rt RuntrackQuerier, vq VerdictQuerier, pq PortfolioQuerier) (result, tier string, evidence *GateEvidence, err error) {
	if cfg.DisableAll {
		return GateNone, TierNone, nil, nil
	}

	// Determine tier from priority
	switch {
	case cfg.Priority <= 1:
		tier = TierHard
	case cfg.Priority <= 3:
		tier = TierSoft
	default:
		return GateNone, TierNone, nil, nil
	}

	// Look up rules for this transition
	rules, ok := gateRules[[2]string{from, to}]
	if !ok {
		rules = nil
	}

	// Portfolio runs: inject children_at_phase check for every transition
	isPortfolio := run.ProjectDir == ""
	if isPortfolio {
		rules = append(rules, gateRule{check: CheckChildrenAtPhase, phase: to})
	}

	if len(rules) == 0 {
		return GatePass, tier, nil, nil
	}

	evidence = &GateEvidence{}
	allPass := true

	for _, rule := range rules {
		cond := GateCondition{
			Check: rule.check,
			Phase: rule.phase,
		}

		switch rule.check {
		case CheckArtifactExists:
			if rt == nil {
				cond.Result = GateFail
				cond.Detail = "no runtrack querier provided"
				allPass = false
				break
			}
			count, qerr := rt.CountArtifacts(ctx, run.ID, rule.phase)
			if qerr != nil {
				return "", "", nil, fmt.Errorf("gate check: %w", qerr)
			}
			cond.Count = &count
			if count > 0 {
				cond.Result = GatePass
			} else {
				cond.Result = GateFail
				cond.Detail = fmt.Sprintf("no artifacts found for phase %q", rule.phase)
				allPass = false
			}

		case CheckAgentsComplete:
			if rt == nil {
				cond.Result = GateFail
				cond.Detail = "no runtrack querier provided"
				allPass = false
				break
			}
			count, qerr := rt.CountActiveAgents(ctx, run.ID)
			if qerr != nil {
				return "", "", nil, fmt.Errorf("gate check: %w", qerr)
			}
			cond.Count = &count
			if count == 0 {
				cond.Result = GatePass
			} else {
				cond.Result = GateFail
				cond.Detail = fmt.Sprintf("%d agents still active", count)
				allPass = false
			}

		case CheckVerdictExists:
			if vq == nil {
				cond.Result = GateFail
				cond.Detail = "no verdict querier provided"
				allPass = false
				break
			}
			scopeID := ""
			if run.ScopeID != nil {
				scopeID = *run.ScopeID
			}
			has, qerr := vq.HasVerdict(ctx, scopeID)
			if qerr != nil {
				return "", "", nil, fmt.Errorf("gate check: %w", qerr)
			}
			if has {
				cond.Result = GatePass
			} else {
				cond.Result = GateFail
				cond.Detail = "no passing verdict found"
				allPass = false
			}

		case CheckChildrenAtPhase:
			if pq == nil {
				cond.Result = GateFail
				cond.Detail = "no portfolio querier provided"
				allPass = false
				break
			}
			children, qerr := pq.GetChildren(ctx, run.ID)
			if qerr != nil {
				return "", "", nil, fmt.Errorf("gate check: %w", qerr)
			}
			behind := 0
			for _, child := range children {
				if IsTerminalStatus(child.Status) && child.Status != StatusActive {
					continue // completed/cancelled children don't block
				}
				childChain := ResolveChain(child)
				targetIdx := ChainPhaseIndex(childChain, rule.phase)
				if targetIdx < 0 {
					continue // child's chain doesn't have this phase — treat as past it
				}
				childIdx := ChainPhaseIndex(childChain, child.Phase)
				if childIdx < targetIdx {
					behind++
				}
			}
			count := behind
			cond.Count = &count
			if behind == 0 {
				cond.Result = GatePass
			} else {
				cond.Result = GateFail
				cond.Detail = fmt.Sprintf("%d children behind phase %q", behind, rule.phase)
				allPass = false
			}

		default:
			cond.Result = GateFail
			cond.Detail = fmt.Sprintf("unknown check type: %q", rule.check)
			allPass = false
		}

		evidence.Conditions = append(evidence.Conditions, cond)
	}

	if allPass {
		return GatePass, tier, evidence, nil
	}
	return GateFail, tier, evidence, nil
}

// EvaluateGate performs a dry-run gate check for the next transition.
// This is the public entry point used by `ic gate check`.
func EvaluateGate(ctx context.Context, store *Store, runID string, cfg GateConfig, rt RuntrackQuerier, vq VerdictQuerier, pq PortfolioQuerier) (*GateCheckResult, error) {
	run, err := store.Get(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("evaluate gate: %w", err)
	}
	if IsTerminalStatus(run.Status) {
		return nil, ErrTerminalRun
	}

	chain := ResolveChain(run)
	if ChainIsTerminal(chain, run.Phase) {
		return nil, ErrTerminalPhase
	}

	toPhase, err := ChainNextPhase(chain, run.Phase)
	if err != nil {
		return nil, fmt.Errorf("evaluate gate: %w", err)
	}
	result, tier, evidence, err := evaluateGate(ctx, run, cfg, run.Phase, toPhase, rt, vq, pq)
	if err != nil {
		return nil, fmt.Errorf("evaluate gate: %w", err)
	}

	return &GateCheckResult{
		RunID:     runID,
		FromPhase: run.Phase,
		ToPhase:   toPhase,
		Result:    result,
		Tier:      tier,
		Evidence:  evidence,
	}, nil
}

// GateRulesInfo returns a list of all gate rules for display purposes.
func GateRulesInfo() []struct {
	From   string
	To     string
	Checks []struct {
		Check string
		Phase string
	}
} {
	var rules []struct {
		From   string
		To     string
		Checks []struct {
			Check string
			Phase string
		}
	}

	// Iterate in phase order for deterministic output
	for i := 0; i < len(DefaultPhaseChain)-1; i++ {
		from := DefaultPhaseChain[i]
		to := DefaultPhaseChain[i+1]
		gr, ok := gateRules[[2]string{from, to}]
		if !ok {
			continue
		}
		entry := struct {
			From   string
			To     string
			Checks []struct {
				Check string
				Phase string
			}
		}{From: from, To: to}
		for _, r := range gr {
			entry.Checks = append(entry.Checks, struct {
				Check string
				Phase string
			}{Check: r.check, Phase: r.phase})
		}
		rules = append(rules, entry)
	}
	return rules
}
