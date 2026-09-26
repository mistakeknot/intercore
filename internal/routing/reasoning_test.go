package routing

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestUnavailableModelsSurviveDecisionContextRoundTrip(t *testing.T) {
	before := DecisionContext{AvailableModels: []string{}}
	raw, err := json.Marshal(before)
	if err != nil {
		t.Fatal(err)
	}
	var after DecisionContext
	if err := json.Unmarshal(raw, &after); err != nil {
		t.Fatal(err)
	}
	if after.AvailableModels == nil {
		t.Fatal("known absent model access became unprobed access")
	}
	if _, err := NewResolver(reasoningConfig(t)).ResolveDecision("planning", "", "", after); err == nil {
		t.Fatal("serialized absent model access admitted work")
	}
}

func reasoningConfig(t *testing.T) *Config {
	t.Helper()
	p := filepath.Join(t.TempDir(), "routing.yaml")
	err := os.WriteFile(p, []byte(`reasoning:
  frontier_models: [gpt-6-astra, claude-fable-5-1]
  frontier_reasons: [unresolved-success-criteria, foundational-invariants, broad-consequences, difficult-verification, capability-failure]
  dual_review_reasons: [foundational-invariants, broad-consequences]
  strikes: 2
dispatch:
  model_aliases: {fable: claude-fable-5-1}
  roles: {planning: sol, frontier-planning: astra, plan-review: fable, escalation: astra, routine-execution: sol, deep-execution: astra}
  tiers:
    sol: {model: gpt-5.6-sol, backend: codex, reasoning_effort: high}
    astra: {model: gpt-6-astra, backend: codex, reasoning_effort: xhigh, fallbacks: [fable, sol]}
    fable: {model: fable, backend: claude, reasoning_effort: high, fallbacks: [astra, sol]}
`), 0600)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(p, "")
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestReasoningContract(t *testing.T) {
	r := NewResolver(reasoningConfig(t))
	c := DecisionContext{Reasons: []string{"foundational-invariants"}, Rationale: "Changes the shared admission contract"}
	got, err := r.ResolveDecision("planning", "", "", c)
	if err != nil {
		t.Fatal(err)
	}
	if got.Profile.Model != "gpt-6-astra" || got.ReviewRequirement != "other-frontier" || got.PolicyHash == "" || got.Profile.ReasoningEffort != "xhigh" {
		t.Fatalf("incomplete contract: %+v", got)
	}
	if len(got.FallbackChain) != 1 || len(got.Excluded) != 1 {
		t.Fatalf("frontier floor lost: %+v", got)
	}
	review, err := r.ResolveDecision("plan-review", "anthropic/fable", "", c)
	if err != nil || review.Profile.Model != "gpt-6-astra" || len(review.FallbackChain) != 0 {
		t.Fatalf("independence: %+v, %v", review, err)
	}
	c.AvailableModels = []string{"gpt-5.6-sol"}
	if _, err := r.ResolveDecision("planning", "", "", c); err == nil {
		t.Fatal("silently downgraded without frontier access")
	}
}

func TestReasoningJudgmentAndHandoff(t *testing.T) {
	r := NewResolver(reasoningConfig(t))
	got, err := r.ResolveDecision("planning", "", "", DecisionContext{Domain: "games"})
	if err != nil || got.Profile.Model != "gpt-5.6-sol" {
		t.Fatalf("domain automatically elevated: %+v %v", got, err)
	}
	if _, err := r.ResolveDecision("planning", "", "", DecisionContext{Reasons: []string{"games"}, Rationale: "domain"}); err == nil {
		t.Fatal("accepted unknown classification")
	}
	c := DecisionContext{Reasons: []string{"difficult-verification"}, Rationale: "requires experiments"}
	got, err = r.ResolveDecision("routine-execution", "", "", c)
	if err != nil || got.Profile.Model != "gpt-6-astra" {
		t.Fatalf("lost frontier in changing plan: %+v %v", got, err)
	}
	c.Handoff = &ReasoningHandoff{Decisions: "frozen", Constraints: "bounded", Verification: "experiment acceptance", Escalation: "premise changes"}
	got, err = r.ResolveDecision("routine-execution", "", "", c)
	if err != nil || got.Profile.Model != "gpt-5.6-sol" {
		t.Fatalf("handoff rejected: %+v %v", got, err)
	}
	c.InvestigationActive = true
	got, err = r.ResolveDecision("routine-execution", "", "", c)
	if err != nil || got.Profile.Model != "gpt-6-astra" {
		t.Fatalf("active investigation demoted: %+v %v", got, err)
	}
}

func TestSelectedPolicyIndependentOfWorkingDirectory(t *testing.T) {
	cfg := reasoningConfig(t)
	t.Setenv("CLAVAIN_ROUTING_POLICY", cfg.PolicySource)
	t.Chdir(t.TempDir())
	p, err := ResolvePolicyPath("")
	if err != nil || p != cfg.PolicySource {
		t.Fatalf("outside checkout: %s %v", p, err)
	}
	if _, err := ResolvePolicyPath(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("invalid explicit policy silently fell back")
	}
}

func TestPolicyPathPreservesSymlinkParentTraversal(t *testing.T) {
	root := t.TempDir()
	installation := filepath.Join(root, "standalone install")
	for _, dir := range []string{"skills", "config"} {
		if err := os.MkdirAll(filepath.Join(installation, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	policy := filepath.Join(installation, "config", "routing.yaml")
	if err := os.WriteFile(policy, []byte("version: 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(installation, "skills"), filepath.Join(root, "clavain")); err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(policy)
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	// Join/Abs would clean clavain/.. before following the installation link.
	for _, path := range []string{root + "/clavain/../config/routing.yaml", "clavain/../config/routing.yaml"} {
		got, err := ResolvePolicyPath(path)
		if err != nil || got != want {
			t.Errorf("policy %q = %q, %v; want %q", path, got, err, want)
		}
	}
}

func TestReasoningContractRecomputesReasonWhenItExcludesTheReorderedPrimary(t *testing.T) {
	// The cross-lab reorder promotes Sol (a non-Claude frontier lab) ahead of
	// Opus for a Claude producer. But the reasoning contract's availability
	// filter then drops Sol, reverting the primary to Opus — the same pick the
	// unreordered policy would have made. "cross_lab_reorder" no longer
	// explains that pick; it must be recomputed to "reasoning_contract".
	c := DecisionContext{AvailableModels: []string{"claude-opus-5", "claude-sonnet-5", "kimi-code/k3", "claude-fable-5-1"}}
	got, err := NewResolver(crossLabConfig()).ResolveDecision("validation", "claude-fable-5-1", "", c)
	if err != nil {
		t.Fatal(err)
	}
	if got.Profile.Model != "claude-opus-5" {
		t.Fatalf("selected %q, want claude-opus-5", got.Profile.Model)
	}
	if got.CrossLabReorder != nil {
		t.Fatalf("the promoted seat was excluded; no reorder should remain: %#v", got.CrossLabReorder)
	}
	if got.FallbackReason != "reasoning_contract" {
		t.Fatalf("FallbackReason = %q, want reasoning_contract", got.FallbackReason)
	}
}

// crossLabConfigOpusFallsBackToSolFirst mirrors crossLabConfig but lists Sol
// ahead of Sonnet in Opus's fallbacks, so a cross-lab reorder that only
// promotes Sol (Case B) or Sol and Kimi (Case C) leaves Opus itself excluded
// deeper in the chain rather than moved. This isolates "the policy primary
// (Opus) got excluded by the contract" from "the reorder's primary got
// excluded by the contract", which TestReasoningContractRecomputesReasonWhenItExcludesTheReorderedPrimary
// already covers.
func crossLabConfigOpusFallsBackToSolFirst() *Config {
	return &Config{
		Reasoning: ReasoningPolicy{FrontierModels: []string{"gpt-6-astra", "claude-fable-5-1", "claude-opus-5"}},
		Dispatch: DispatchConfig{
			ModelAliases:  map[string]string{"opus": "claude-opus-5"},
			Roles:         map[string]string{"validation": "opus"},
			CrossLabFirst: []string{"validation"},
			Tiers: map[string]DispatchProfile{
				"opus":   {Backend: "claude", Model: "opus", ReasoningEffort: "high", Fallbacks: []string{"sol", "sonnet", "kimi"}},
				"sonnet": {Backend: "claude", Model: "claude-sonnet-5", ReasoningEffort: "high"},
				"sol":    {Backend: "codex", Model: "gpt-5.6-sol", ReasoningEffort: "high"},
				"kimi":   {Backend: "kimi", Model: "kimi-code/k3", ReasoningEffort: "high"},
			},
		},
	}
}

func TestReasoningContractRestoresReasonWhenTrimmedReorderCollapsesToNil(t *testing.T) {
	// Case B (R2-1): Opus is unavailable. The reorder already promoted Sol
	// ahead of Opus, so Sol was primary before and after the contract filter —
	// the reorder itself never moves. But Opus, the un-reordered policy
	// primary, was still excluded by the contract, so this must still read
	// "reasoning_contract", not "" (comparing only against the post-reorder
	// primary would wrongly clear it).
	c := DecisionContext{AvailableModels: []string{"gpt-5.6-sol", "claude-sonnet-5", "kimi-code/k3", "claude-fable-5-1"}}
	got, err := NewResolver(crossLabConfigOpusFallsBackToSolFirst()).ResolveDecision("validation", "claude-fable-5-1", "", c)
	if err != nil {
		t.Fatal(err)
	}
	if got.Profile.Model != "gpt-5.6-sol" {
		t.Fatalf("selected %q, want gpt-5.6-sol", got.Profile.Model)
	}
	if got.CrossLabReorder != nil {
		t.Fatalf("trimmed reorder should collapse to nil (Sol unmoved by exclusion): %#v", got.CrossLabReorder)
	}
	if got.FallbackReason != "reasoning_contract" {
		t.Fatalf("FallbackReason = %q, want reasoning_contract", got.FallbackReason)
	}
}

func TestReasoningContractStaysReasoningContractWhenTrimmedReorderKeepsSamePrimary(t *testing.T) {
	// Case C (R2-1): same as Case B, but Kimi's lab is also a frontier lab, so
	// the reorder additionally promotes Kimi ahead of Sonnet. The trimmed
	// reorder survives (From[0]==To[0]=="sol", but the rest of the order still
	// differs), which used to make the reset think "cross_lab_reorder" was
	// still valid. It must resolve to "reasoning_contract": Opus, the
	// un-reordered policy primary, was excluded by the contract.
	cfg := crossLabConfigOpusFallsBackToSolFirst()
	cfg.Reasoning.FrontierModels = append(cfg.Reasoning.FrontierModels, "kimi-code/k3")
	c := DecisionContext{AvailableModels: []string{"gpt-5.6-sol", "claude-sonnet-5", "kimi-code/k3", "claude-fable-5-1"}}
	got, err := NewResolver(cfg).ResolveDecision("validation", "claude-fable-5-1", "", c)
	if err != nil {
		t.Fatal(err)
	}
	if got.Profile.Model != "gpt-5.6-sol" {
		t.Fatalf("selected %q, want gpt-5.6-sol", got.Profile.Model)
	}
	if got.CrossLabReorder == nil || got.CrossLabReorder.From[0] != got.CrossLabReorder.To[0] {
		t.Fatalf("expected a surviving reorder with an unmoved primary: %#v", got.CrossLabReorder)
	}
	if got.FallbackReason != "reasoning_contract" {
		t.Fatalf("FallbackReason = %q, want reasoning_contract", got.FallbackReason)
	}
}

func TestAlternateProfileIsScopedAndCannotDemoteFrontier(t *testing.T) {
	cfg := reasoningConfig(t)
	cfg.Reasoning.Profiles = map[string]PolicyProfile{"pilot": {Scope: "campaign", Roles: map[string]string{"frontier-planning": "sol"}}}
	r := NewResolver(cfg)
	c := DecisionContext{Reasons: []string{"broad-consequences"}, Rationale: "many consumers"}
	if _, err := r.ResolveDecision("planning", "", "pilot", c); err == nil {
		t.Fatal("unscoped pilot accepted")
	}
	c.Scope = "campaign"
	if _, err := r.ResolveDecision("planning", "", "pilot", c); err == nil {
		t.Fatal("pilot demoted required frontier")
	}
}
