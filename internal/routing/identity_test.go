package routing

import "testing"

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
