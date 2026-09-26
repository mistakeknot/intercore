package routing

import (
	"slices"
	"testing"
)

func identityConfig() *Config {
	return &Config{Dispatch: DispatchConfig{
		ModelAliases: map[string]string{"fable": "claude-fable-5-1", "gpt-5.6": "gpt-5.6-sol"},
		Roles:        map[string]string{"validation": "fable"},
		Tiers: map[string]DispatchProfile{
			"fable": {Backend: "claude", Model: "fable", Fallbacks: []string{"sol", "astra"}},
			"sol":   {Backend: "codex", Model: "gpt-5.6-sol"},
			"astra": {Backend: "codex", Model: "gpt-6-astra"},
		},
	}}
}

func TestValidatorExcludesCanonicalProducerAndPreservesEvidence(t *testing.T) {
	r := NewResolver(identityConfig())
	for _, producer := range []string{"fable", "claude-fable-5-1", "anthropic/claude-fable-5-1[1m]", "claude:claude-fable-5-1-20260901"} {
		t.Run(producer, func(t *testing.T) {
			got, err := r.ResolveDispatchRoleForProducer("validation", producer)
			if err != nil {
				t.Fatal(err)
			}
			if got.Profile.Model != "gpt-5.6-sol" || got.ProducerModel != "claude-fable-5-1" {
				t.Fatalf("wrong separation: %#v", got)
			}
			if len(got.Excluded) != 1 || got.Excluded[0].Reason != "producer_model_conflict" || got.Excluded[0].Profile.Model != "claude-fable-5-1" {
				t.Fatalf("lost exclusion evidence: %#v", got.Excluded)
			}
			if got.ValidatorRelationship != "different-model" || got.FallbackReason != "producer_model_conflict" || len(got.FallbackChain) != 1 {
				t.Fatalf("lost routing evidence: %#v", got)
			}
		})
	}
}

func TestValidatorFiltersMatchingFallbacksAndPinsAliases(t *testing.T) {
	got, err := NewResolver(identityConfig()).ResolveDispatchRoleForProducer("validation", "openai:gpt-5.6-20260901")
	if err != nil {
		t.Fatal(err)
	}
	if got.Profile.Model != "claude-fable-5-1" || len(got.FallbackChain) != 1 || got.FallbackChain[0].Profile.Model != "gpt-6-astra" {
		t.Fatalf("unsafe chain: %#v", got)
	}
}

func TestValidatorFailsClosedWithoutKnownProducerOrDistinctModel(t *testing.T) {
	for _, producer := range []string{"", "unknown-alias"} {
		if _, err := NewResolver(identityConfig()).ResolveDispatchRoleForProducer("validation", producer); err == nil {
			t.Fatalf("accepted ambiguous producer %q", producer)
		}
	}
	cfg := identityConfig()
	cfg.Dispatch.Tiers["fable"] = DispatchProfile{Model: "fable", Backend: "claude"}
	if _, err := NewResolver(cfg).ResolveDispatchRoleForProducer("validation", "fable"); err == nil {
		t.Fatal("accepted same-model validator")
	}
	cfg.Dispatch.ModelAliases["fable"] = "fable"
	if _, err := NewResolver(cfg).ResolveDispatchRoleForProducer("validation", "gpt-6-astra"); err == nil {
		t.Fatal("accepted cyclic alias")
	}
}

func crossLabConfig() *Config {
	return &Config{
		Reasoning: ReasoningPolicy{FrontierModels: []string{"gpt-6-astra", "claude-fable-5-1", "claude-opus-5"}},
		Dispatch: DispatchConfig{
			ModelAliases:  map[string]string{"opus": "claude-opus-5"},
			Roles:         map[string]string{"validation": "opus", "plan-review": "opus"},
			CrossLabFirst: []string{"validation"},
			Tiers: map[string]DispatchProfile{
				"opus":   {Backend: "claude", Model: "opus", ReasoningEffort: "high", Fallbacks: []string{"sonnet", "sol", "kimi"}},
				"sonnet": {Backend: "claude", Model: "claude-sonnet-5", ReasoningEffort: "high"},
				"sol":    {Backend: "codex", Model: "gpt-5.6-sol", ReasoningEffort: "high"},
				"kimi":   {Backend: "kimi", Model: "kimi-code/k3", ReasoningEffort: "high"},
			},
		},
	}
}

func refsOf(got ResolvedDispatch) []string {
	out := []string{got.ProfileRef}
	for _, c := range got.FallbackChain {
		out = append(out, c.ProfileRef)
	}
	return out
}

func TestCrossLabFirstPrefersAnotherFrontierLabForClaudeWork(t *testing.T) {
	got, err := NewResolver(crossLabConfig()).ResolveDispatchRoleForProducer("validation", "claude-fable-5-1")
	if err != nil {
		t.Fatal(err)
	}
	// Opus before Sonnet is kept; Kimi is not a frontier lab and keeps its place.
	if want := []string{"sol", "opus", "sonnet", "kimi"}; !slices.Equal(refsOf(got), want) {
		t.Fatalf("order %v, want %v", refsOf(got), want)
	}
	if got.CrossLabReorder == nil || !slices.Equal(got.CrossLabReorder.From, []string{"opus", "sonnet", "sol", "kimi"}) {
		t.Fatalf("reorder not recorded: %#v", got.CrossLabReorder)
	}
	if got.FallbackReason != "cross_lab_reorder" {
		t.Fatalf("reorder changed the primary but FallbackReason = %q", got.FallbackReason)
	}
}

func TestCrossLabFirstReasonYieldsToProducerConflict(t *testing.T) {
	// Opus is the producer here, so it is excluded outright (producer_model_conflict)
	// before the reorder ever runs. The reorder still promotes Sol ahead of
	// Sonnet, but the more important producer-conflict reason must survive.
	got, err := NewResolver(crossLabConfig()).ResolveDispatchRoleForProducer("validation", "claude-opus-5")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"sol", "sonnet", "kimi"}; !slices.Equal(refsOf(got), want) {
		t.Fatalf("order %v, want %v", refsOf(got), want)
	}
	if got.CrossLabReorder == nil {
		t.Fatalf("expected a recorded reorder")
	}
	if got.FallbackReason != "producer_model_conflict" {
		t.Fatalf("producer conflict reason was clobbered: %q", got.FallbackReason)
	}
}

func TestCrossLabFirstKeepsPolicyOrderForOtherLabWork(t *testing.T) {
	got, err := NewResolver(crossLabConfig()).ResolveDispatchRoleForProducer("validation", "gpt-6-astra")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"opus", "sonnet", "sol", "kimi"}; !slices.Equal(refsOf(got), want) || got.CrossLabReorder != nil {
		t.Fatalf("order %v reorder %#v", refsOf(got), got.CrossLabReorder)
	}
}

func TestCrossLabFirstStillExcludesTheProducer(t *testing.T) {
	got, err := NewResolver(crossLabConfig()).ResolveDispatchRoleForProducer("validation", "gpt-5.6-sol")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"opus", "sonnet", "kimi"}; !slices.Equal(refsOf(got), want) {
		t.Fatalf("order %v, want %v", refsOf(got), want)
	}
}

func TestCrossLabFirstAppliesOnlyToListedRoles(t *testing.T) {
	got, err := NewResolver(crossLabConfig()).ResolveDispatchRoleForProducer("plan-review", "claude-fable-5-1")
	if err != nil {
		t.Fatal(err)
	}
	if refsOf(got)[0] != "opus" || got.CrossLabReorder != nil {
		t.Fatalf("unlisted role reordered: %v", refsOf(got))
	}
}

func TestCrossLabFirstFallsBackToOpusWhenCodexIsOut(t *testing.T) {
	c := DecisionContext{AvailableModels: []string{"claude-opus-5", "claude-sonnet-5", "kimi-code/k3", "claude-fable-5-1"}}
	got, err := NewResolver(crossLabConfig()).ResolveDecision("validation", "claude-fable-5-1", "", c)
	if err != nil {
		t.Fatal(err)
	}
	if got.Profile.Model != "claude-opus-5" {
		t.Fatalf("selected %q, want Opus", got.Profile.Model)
	}
	// Sol was excluded as unavailable, so no reorder remains over the kept seats.
	if got.CrossLabReorder != nil {
		t.Fatalf("receipt names an excluded seat: %#v", got.CrossLabReorder)
	}
	got, err = NewResolver(crossLabConfig()).ResolveDecision("validation", "claude-opus-5", "", DecisionContext{})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"sol", "sonnet", "kimi"}; !slices.Equal(refsOf(got.ResolvedDispatch), want) || got.CrossLabReorder.To[0] != "sol" {
		t.Fatalf("opus producer order %v", refsOf(got.ResolvedDispatch))
	}
}
