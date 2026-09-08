package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/mistakeknot/intercore/internal/routing"
	"github.com/mistakeknot/intercore/internal/state"
)

// FailureMode is the closed vocabulary for why an attempt failed.
// Enum serves Phase-4 aggregation; Detail (free text) serves lesson transport.
type FailureMode string

const (
	FailTimeout        FailureMode = "timeout"
	FailError          FailureMode = "error"
	FailVerdict        FailureMode = "verdict-fail"
	FailCriteria       FailureMode = "criteria-fail"
	FailUnknown        FailureMode = "unknown"
	FailCapability     FailureMode = "capability-failure"
	FailPremise        FailureMode = "premise-failure"
	FailAuthentication FailureMode = "authentication"
	FailRateLimit      FailureMode = "rate-limit"
	FailPermission     FailureMode = "permission"
	FailInfrastructure FailureMode = "infrastructure"
)

func ParseFailureMode(s string) FailureMode {
	switch FailureMode(s) {
	case FailTimeout, FailError, FailVerdict, FailCriteria, FailCapability, FailPremise, FailAuthentication, FailRateLimit, FailPermission, FailInfrastructure:
		return FailureMode(s)
	default:
		return FailUnknown
	}
}

// EscalationPolicy configures the two-strikes ladder.
type EscalationPolicy struct {
	Routing        *routing.Config
	PolicyProfile  string
	Context        routing.DecisionContext
	Retry          RetryPolicy // per-rung retry behavior (backoff etc.)
	Ladder         []string    // capability ladder, low→high
	StrikesPerRung int         // failures at a rung before stepping up (doctrine: 2)
	MaxEscalations int         // total rung-steps allowed per chain (guard vs oscillation)
}

// DefaultEscalationPolicy implements the capability-routing doctrine:
// two strikes at the base tier, then one attempt per higher rung.
func DefaultEscalationPolicy() EscalationPolicy {
	return EscalationPolicy{
		// MaxRetries must cover base strikes + one attempt per remaining rung
		// (f-003: the shipped default of 3 would block the second escalation).
		Retry:          RetryPolicy{MaxRetries: 4, BaseBackoff: 5 * time.Second, MaxBackoff: 5 * time.Minute, BackoffFactor: 2.0, RetryOnTimeout: true},
		Ladder:         nil, // capability assignments come from Clavain, never the kernel
		StrikesPerRung: 2,
		MaxEscalations: 2,
	}
}

// EscalationPolicyFromConfig binds retry mechanics to the selected OS policy.
func EscalationPolicyFromConfig(cfg *routing.Config) EscalationPolicy {
	p := DefaultEscalationPolicy()
	p.Routing = cfg
	if cfg.Reasoning.Strikes > 0 {
		p.StrikesPerRung = cfg.Reasoning.Strikes
	}
	return p
}

// Legacy callers may explicitly supply a ladder. Unknown models fail closed.
func (p EscalationPolicy) nextRungModel(current string, strikes int) (string, bool) {
	if strikes < p.StrikesPerRung {
		return current, true
	}
	for i, m := range p.Ladder {
		if m == current && i+1 < len(p.Ladder) {
			return p.Ladder[i+1], true
		}
	}
	return "", false
}

func capabilityStrike(mode FailureMode) bool {
	return mode == FailCapability || mode == FailPremise || mode == FailCriteria || mode == FailVerdict
}

// ChainState is the durable per-chain record (survives fresh re-triggers —
// f-024/f-025: dispatch rows cannot carry this because a new top-level
// Create() mints a fresh root with RetryCount=0).
type ChainState struct {
	ChainKey      string                     `json:"chain_key"`
	Dispatches    []string                   `json:"dispatches"`
	Models        []string                   `json:"models"`
	Failures      []string                   `json:"failures"` // "<mode>: <detail>" per attempt
	CurrentModel  string                     `json:"current_model"`
	StrikesAtRung int                        `json:"strikes_at_rung"`
	Escalations   int                        `json:"escalations"`
	Exhausted     bool                       `json:"exhausted"`
	Decision      *routing.ReasoningDecision `json:"decision,omitempty"`
	Retries       map[string]string          `json:"retries,omitempty"`
}

const chainScope = "escalation"

func chainStateKey(chainKey string) string { return "dispatch.chain." + chainKey }

// LoadChainState fetches chain state; a missing key returns a fresh state.
func LoadChainState(ctx context.Context, st *state.Store, chainKey string) (*ChainState, error) {
	raw, err := st.Get(ctx, chainStateKey(chainKey), chainScope)
	if err != nil {
		if err == state.ErrNotFound {
			return &ChainState{ChainKey: chainKey}, nil
		}
		return nil, err
	}
	var cs ChainState
	if uerr := json.Unmarshal(raw, &cs); uerr != nil {
		return nil, uerr
	}
	return &cs, nil
}

func SaveChainState(ctx context.Context, st *state.Store, cs *ChainState) error {
	raw, err := json.Marshal(cs)
	if err != nil {
		return err
	}
	return st.Set(ctx, chainStateKey(cs.ChainKey), chainScope, raw, 0)
}

// EscalationResult reports what RetryWithEscalation decided.
type EscalationResult struct {
	RetryResult
	Model      string                     `json:"model"`
	Escalated  bool                       `json:"escalated"`
	Exhausted  bool                       `json:"exhausted"`
	LessonFile string                     `json:"lesson_file,omitempty"`
	Decision   *routing.ReasoningDecision `json:"decision,omitempty"`
}

// RetryWithEscalation is the two-strikes ladder entry point. It records the
// failure on the chain, decides same-model retry vs escalation vs exhaustion,
// builds a lesson-carrying prompt (f-031), and creates the retry dispatch.
// The caller starts the actual process (same contract as Retry).
func RetryWithEscalation(ctx context.Context, store *Store, st *state.Store, originalID string, policy EscalationPolicy, chainKey string, mode FailureMode, detail string) (*EscalationResult, error) {
	orig, err := store.Get(ctx, originalID)
	if err != nil {
		return nil, fmt.Errorf("escalate: get original: %w", err)
	}
	if !ShouldRetry(orig, policy.Retry) {
		return nil, &SpawnRejection{Reason: "not_retryable"}
	}
	origModel := ""
	if orig.Model != nil {
		origModel = *orig.Model
	}
	if chainKey == "" {
		chainKey = originalID // degraded mode: chain = this dispatch lineage only
	}
	cs, err := LoadChainState(ctx, st, chainKey)
	if err != nil {
		return nil, fmt.Errorf("escalate: chain state: %w", err)
	}
	if cs.Exhausted {
		return nil, fmt.Errorf("escalate: chain %s already exhausted — surface to human (lesson chain in state key %s)", chainKey, chainStateKey(chainKey))
	}
	if retryID := cs.Retries[originalID]; retryID != "" {
		return nil, fmt.Errorf("escalate: attempt already retried as %s", retryID)
	}
	if cs.CurrentModel == "" {
		cs.CurrentModel = origModel
	}

	// Record all failures; only capability and premise failures consume strikes.
	alreadyRecorded := false
	for _, id := range cs.Dispatches {
		if id == originalID {
			alreadyRecorded = true
		}
	}
	if !alreadyRecorded {
		cs.Dispatches = append(cs.Dispatches, originalID)
		cs.Models = append(cs.Models, cs.CurrentModel)
		cs.Failures = append(cs.Failures, string(mode)+": "+detail)
		if capabilityStrike(mode) {
			cs.StrikesAtRung++
		}
		if mode == FailPremise {
			cs.StrikesAtRung = policy.StrikesPerRung
		}
	}
	if err := SaveChainState(ctx, st, cs); err != nil {
		return nil, err
	}

	nextModel, ok := cs.CurrentModel, true
	decision, err := loadReasoningDecision(ctx, store, originalID)
	if err != nil {
		return nil, err
	}
	if cs.Decision != nil {
		decision = cs.Decision
	}
	if decision != nil && policy.Routing != nil && decision.PolicyHash != policy.Routing.PolicyHash {
		return nil, fmt.Errorf("escalate: policy changed; reclassify before retry")
	}
	if decision != nil && (decision.RequestedRole == "plan-review" || decision.RequestedRole == "validation" || decision.RequestedRole == "cross-lab-review") && capabilityStrike(mode) && cs.StrikesAtRung >= policy.StrikesPerRung {
		return nil, fmt.Errorf("escalate: review requires a newly resolved independent review contract")
	}
	if capabilityStrike(mode) && cs.StrikesAtRung >= policy.StrikesPerRung {
		if policy.Routing != nil {
			c := policy.Context
			if decision != nil {
				c = decision.Context
				policy.PolicyProfile = decision.PolicyProfile
			}
			found := false
			for _, reason := range c.Reasons {
				if reason == "capability-failure" {
					found = true
				}
			}
			if !found {
				c.Reasons = append(append([]string{}, c.Reasons...), "capability-failure")
			}
			c.Rationale = c.Rationale + "; escalation: " + detail
			resolved, resolveErr := routing.NewResolver(policy.Routing).ResolveDecision("escalation", cs.CurrentModel, policy.PolicyProfile, c)
			if resolveErr != nil {
				return nil, fmt.Errorf("escalate: required frontier unavailable: %w", resolveErr)
			}
			// Avoid oscillating between frontier models already tried in this chain.
			candidates := append([]routing.DispatchCandidate{{ProfileRef: resolved.ProfileRef, Profile: resolved.Profile}}, resolved.FallbackChain...)
			ok = false
			for _, candidate := range candidates {
				seen := false
				for _, m := range cs.Models {
					identity, _ := routing.NewResolver(policy.Routing).CanonicalModelIdentity(m)
					if identity == candidate.Profile.ModelIdentity {
						seen = true
					}
				}
				if !seen {
					resolved.ProfileRef = candidate.ProfileRef
					resolved.Profile = candidate.Profile
					resolved.FallbackChain = nil
					nextModel = candidate.Profile.Model
					decision = &resolved
					ok = true
					break
				}
			}
		} else {
			nextModel, ok = cs.nextModel(policy)
		}
	}
	if !ok {
		cs.Exhausted = true
		lesson := writeLessonChain(orig, cs)
		_ = SaveChainState(ctx, st, cs)
		return &EscalationResult{Exhausted: true, LessonFile: lesson}, fmt.Errorf("escalate: ladder exhausted for chain %s after %d attempts", chainKey, len(cs.Failures))
	}
	escalated := nextModel != cs.CurrentModel
	if escalated {
		if cs.Escalations >= policy.MaxEscalations {
			cs.Exhausted = true
			lesson := writeLessonChain(orig, cs)
			_ = SaveChainState(ctx, st, cs)
			return &EscalationResult{Exhausted: true, LessonFile: lesson}, fmt.Errorf("escalate: MaxEscalations (%d) reached for chain %s", policy.MaxEscalations, chainKey)
		}
		cs.Escalations++
		cs.CurrentModel = nextModel
		cs.StrikesAtRung = 0
	}

	// Lesson transport (f-031): retry prompt = original prompt + prior lesson.
	lessonPrompt := buildLessonPrompt(orig, cs)

	// Create the retry record via the same construction as Retry(), but with
	// the (possibly escalated) model and the lesson prompt.
	attempt := orig.RetryCount + 1
	d := &Dispatch{
		AgentType:        orig.AgentType,
		ProjectDir:       orig.ProjectDir,
		PromptFile:       orig.PromptFile,
		PromptHash:       nil, // prompt mutated by lesson injection
		Name:             orig.Name,
		Sandbox:          orig.Sandbox,
		SandboxSpec:      orig.SandboxSpec,
		TimeoutSec:       orig.TimeoutSec,
		ScopeID:          orig.ScopeID,
		ParentID:         orig.ParentID,
		RetryCount:       attempt,
		ParentDispatchID: originalID,
		SpawnDepth:       orig.SpawnDepth,
	}
	if lessonPrompt != "" {
		d.PromptFile = &lessonPrompt
	}
	if nextModel != "" {
		d.Model = &nextModel
	}
	if decision != nil {
		d.AgentType = decision.Profile.Backend
	}
	newID, cerr := store.admitRetry(ctx, d)
	if cerr != nil {
		return nil, fmt.Errorf("escalate: create: %w", cerr)
	}
	cs.Decision = decision
	if cs.Retries == nil {
		cs.Retries = map[string]string{}
	}
	cs.Retries[originalID] = newID
	if decision != nil {
		if err := recordReasoningDecision(ctx, store, newID, d.ProjectDir, decision); err != nil {
			return nil, err
		}
	}
	if serr := SaveChainState(ctx, st, cs); serr != nil {
		return nil, fmt.Errorf("escalate: save chain: %w", serr)
	}
	backoff := policy.Retry.Backoff(orig.RetryCount)
	return &EscalationResult{
		RetryResult: RetryResult{OriginalID: originalID, NewID: newID, Attempt: attempt, BackoffMs: backoff.Milliseconds()},
		Model:       nextModel,
		Escalated:   escalated,
		Decision:    decision,
	}, nil
}

// nextModel wraps policy.nextRungModel with chain state.
func (cs *ChainState) nextModel(p EscalationPolicy) (string, bool) {
	return p.nextRungModel(cs.CurrentModel, cs.StrikesAtRung)
}

// buildLessonPrompt writes a new prompt file: original prompt + a
// "Prior attempt lesson" section (verdict + output tail), mirroring
// orchestrate.py's summarize_output pattern. Returns new path or "".
func buildLessonPrompt(orig *Dispatch, cs *ChainState) string {
	if orig.PromptFile == nil || *orig.PromptFile == "" {
		return ""
	}
	base, err := os.ReadFile(*orig.PromptFile)
	if err != nil {
		return ""
	}
	var lesson strings.Builder
	lesson.Write(base)
	lesson.WriteString("\n\n## Prior attempt lesson (do not repeat these failures)\n")
	for i, f := range cs.Failures {
		model := ""
		if i < len(cs.Models) {
			model = cs.Models[i]
		}
		lesson.WriteString(fmt.Sprintf("- attempt %d (%s): %s\n", i+1, model, f))
	}
	if orig.VerdictFile != nil && *orig.VerdictFile != "" {
		if v, verr := os.ReadFile(*orig.VerdictFile); verr == nil {
			lesson.WriteString("\n### Last verdict\n```\n" + string(v) + "\n```\n")
		}
	}
	if orig.OutputFile != nil && *orig.OutputFile != "" {
		if o, oerr := os.ReadFile(*orig.OutputFile); oerr == nil {
			lines := strings.Split(string(o), "\n")
			if len(lines) > 50 {
				lines = lines[len(lines)-50:]
			}
			lesson.WriteString("\n### Last output (tail)\n```\n" + strings.Join(lines, "\n") + "\n```\n")
		}
	}
	newPath := fmt.Sprintf("%s.retry%d", *orig.PromptFile, orig.RetryCount+1)
	if werr := os.WriteFile(newPath, []byte(lesson.String()), 0o644); werr != nil {
		return ""
	}
	return newPath
}

// writeLessonChain writes the human-handoff artifact at exhaustion (f-033):
// the per-tier lesson chain, not just a counter.
func writeLessonChain(orig *Dispatch, cs *ChainState) string {
	dir := orig.ProjectDir
	if dir == "" {
		dir = os.TempDir()
	}
	path := fmt.Sprintf("%s/escalation-chain-%s.md", dir, sanitizeKey(cs.ChainKey))
	var b strings.Builder
	b.WriteString(fmt.Sprintf("# Escalation chain exhausted: %s\n\n", cs.ChainKey))
	b.WriteString(fmt.Sprintf("Attempts: %d  Escalations: %d\n\n", len(cs.Failures), cs.Escalations))
	for i, f := range cs.Failures {
		model, id := "", ""
		if i < len(cs.Models) {
			model = cs.Models[i]
		}
		if i < len(cs.Dispatches) {
			id = cs.Dispatches[i]
		}
		b.WriteString(fmt.Sprintf("## Attempt %d — %s (dispatch %s)\n%s\n\n", i+1, model, id, f))
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return ""
	}
	return path
}

func sanitizeKey(k string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, k)
}
