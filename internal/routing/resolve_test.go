package routing

import "testing"

// testConfig returns a Config matching the routing.yaml schema used in production.
func testConfig() *Config {
	return &Config{
		Subagents: SubagentConfig{
			Defaults: SubagentDefaults{
				Model: "sonnet",
				Categories: map[string]string{
					"research": "haiku",
					"review":   "sonnet",
				},
			},
			Phases: map[string]PhaseConfig{
				"plan": {
					Model: "opus",
					Categories: map[string]string{
						"research": "sonnet",
					},
				},
				"execute": {
					Model: "sonnet",
				},
			},
			Overrides: map[string]string{
				"interflux:review:fd-safety": "opus",
			},
		},
		Dispatch: DispatchConfig{
			Tiers: map[string]TierConfig{
				"fast": {Model: "haiku"},
				"deep": {Model: "opus"},
			},
			Fallback: map[string]string{
				"medium": "fast",
			},
		},
		Roles: RolesConfig{
			Roles: map[string]RoleEntry{
				"reviewer": {
					MinModel: "sonnet",
					Agents:   []string{"fd-safety", "fd-correctness"},
				},
			},
		},
	}
}

func TestResolveModelOverride(t *testing.T) {
	r := NewResolver(testConfig())
	got := r.ResolveModel(ResolveOpts{Agent: "interflux:review:fd-safety"})
	if got != "opus" {
		t.Errorf("override: got %q, want opus", got)
	}
}

func TestResolveModelPhaseCategory(t *testing.T) {
	r := NewResolver(testConfig())
	got := r.ResolveModel(ResolveOpts{Phase: "plan", Category: "research"})
	if got != "sonnet" {
		t.Errorf("phase+category: got %q, want sonnet", got)
	}
}

func TestResolveModelPhaseLevel(t *testing.T) {
	r := NewResolver(testConfig())
	got := r.ResolveModel(ResolveOpts{Phase: "plan"})
	if got != "opus" {
		t.Errorf("phase-level: got %q, want opus", got)
	}
}

func TestResolveModelDefaultCategory(t *testing.T) {
	r := NewResolver(testConfig())
	got := r.ResolveModel(ResolveOpts{Category: "research"})
	if got != "haiku" {
		t.Errorf("default category: got %q, want haiku", got)
	}
}

func TestResolveModelDefaultModel(t *testing.T) {
	r := NewResolver(testConfig())
	got := r.ResolveModel(ResolveOpts{})
	if got != "sonnet" {
		t.Errorf("default model: got %q, want sonnet", got)
	}
}

func TestResolveModelUltimateFallback(t *testing.T) {
	cfg := &Config{
		Subagents: SubagentConfig{
			Defaults:  SubagentDefaults{Categories: map[string]string{}},
			Phases:    map[string]PhaseConfig{},
			Overrides: map[string]string{},
		},
	}
	r := NewResolver(cfg)
	got := r.ResolveModel(ResolveOpts{})
	if got != "sonnet" {
		t.Errorf("ultimate fallback: got %q, want sonnet", got)
	}
}

func TestResolveModelInheritSkipped(t *testing.T) {
	cfg := &Config{
		Subagents: SubagentConfig{
			Defaults: SubagentDefaults{
				Model:      "inherit",
				Categories: map[string]string{},
			},
			Phases:    map[string]PhaseConfig{},
			Overrides: map[string]string{},
		},
	}
	r := NewResolver(cfg)
	got := r.ResolveModel(ResolveOpts{})
	if got != "sonnet" {
		t.Errorf("inherit should fall through to sonnet, got %q", got)
	}
}

func TestResolveModelSafetyFloorClamp(t *testing.T) {
	r := NewResolver(testConfig())
	// fd-correctness has floor=sonnet. Default category research=haiku.
	// Should clamp haiku → sonnet.
	got := r.ResolveModel(ResolveOpts{
		Category: "research",
		Agent:    "fd-correctness",
	})
	if got != "sonnet" {
		t.Errorf("safety floor clamp: got %q, want sonnet", got)
	}
}

func TestResolveModelSafetyFloorNamespaceStripping(t *testing.T) {
	r := NewResolver(testConfig())
	// Namespaced agent ID — floor lookup must strip to "fd-correctness"
	got := r.ResolveModel(ResolveOpts{
		Category: "research",
		Agent:    "interflux:review:fd-correctness",
	})
	if got != "sonnet" {
		t.Errorf("namespace stripping floor: got %q, want sonnet", got)
	}
}

func TestResolveModelNoFloorNoClamp(t *testing.T) {
	r := NewResolver(testConfig())
	// Agent with no floor — haiku should pass through
	got := r.ResolveModel(ResolveOpts{
		Category: "research",
		Agent:    "some-editor",
	})
	if got != "haiku" {
		t.Errorf("no floor: got %q, want haiku", got)
	}
}

func TestResolveDispatchTier(t *testing.T) {
	r := NewResolver(testConfig())

	tests := []struct {
		tier string
		want string
	}{
		{"fast", "haiku"},
		{"deep", "opus"},
		{"medium", "haiku"}, // fallback: medium → fast → haiku
		{"unknown", ""},
	}
	for _, tt := range tests {
		got := r.ResolveDispatchTier(tt.tier)
		if got != tt.want {
			t.Errorf("ResolveDispatchTier(%q) = %q, want %q", tt.tier, got, tt.want)
		}
	}
}

func TestResolveDispatchTierCycleProtection(t *testing.T) {
	cfg := &Config{
		Dispatch: DispatchConfig{
			Tiers:    map[string]TierConfig{},
			Fallback: map[string]string{"a": "b", "b": "c", "c": "a"},
		},
	}
	r := NewResolver(cfg)
	// Should terminate within 3 hops, not loop forever
	got := r.ResolveDispatchTier("a")
	if got != "" {
		t.Errorf("cycle: got %q, want empty", got)
	}
}

func TestResolveBatch(t *testing.T) {
	r := NewResolver(testConfig())
	agents := []string{"fd-safety", "fd-correctness", "best-practices-researcher", "some-agent"}
	result := r.ResolveBatch(agents, "plan")

	// fd-safety → infers review category, namespaced to interflux:review:fd-safety → override=opus
	if v := result["fd-safety"]; v != "opus" {
		t.Errorf("batch fd-safety = %q, want opus", v)
	}

	// fd-correctness → infers review, no override, plan phase review not set → plan.model=opus
	// But floor=sonnet, opus >= sonnet, so opus passes through
	if v := result["fd-correctness"]; v != "opus" {
		t.Errorf("batch fd-correctness = %q, want opus", v)
	}

	// best-practices-researcher → infers research, plan.categories.research=sonnet
	// No floor, so sonnet passes through
	if v := result["best-practices-researcher"]; v != "sonnet" {
		t.Errorf("batch researcher = %q, want sonnet", v)
	}

	// some-agent → no category inferred, plan.model=opus
	if v := result["some-agent"]; v != "opus" {
		t.Errorf("batch some-agent = %q, want opus", v)
	}
}

func TestInferCategory(t *testing.T) {
	tests := []struct {
		agent string
		want  string
	}{
		{"best-practices-researcher", "research"},
		{"repo-research-analyst", "research"},
		{"fd-safety", "review"},
		{"fd-correctness", "review"},
		{"some-agent", ""},
	}
	for _, tt := range tests {
		got := inferCategory(tt.agent)
		if got != tt.want {
			t.Errorf("inferCategory(%q) = %q, want %q", tt.agent, got, tt.want)
		}
	}
}

func TestInferAgentID(t *testing.T) {
	tests := []struct {
		agent string
		want  string
	}{
		{"best-practices-researcher", "interflux:research:best-practices-researcher"},
		{"repo-research-analyst", "interflux:research:repo-research-analyst"},
		{"fd-safety", "interflux:review:fd-safety"},
		{"some-agent", "some-agent"},
	}
	for _, tt := range tests {
		got := inferAgentID(tt.agent)
		if got != tt.want {
			t.Errorf("inferAgentID(%q) = %q, want %q", tt.agent, got, tt.want)
		}
	}
}

func TestApplyFloorUnknownTier(t *testing.T) {
	// If floor is an unknown tier string, it should not clamp
	cfg := &Config{
		Roles: RolesConfig{
			Roles: map[string]RoleEntry{
				"weird": {
					MinModel: "gpt4", // unknown tier
					Agents:   []string{"test-agent"},
				},
			},
		},
	}
	cfg.Subagents.Defaults.Categories = map[string]string{}
	cfg.Subagents.Phases = map[string]PhaseConfig{}
	cfg.Subagents.Overrides = map[string]string{}

	r := NewResolver(cfg)
	got := r.applyFloor("test-agent", "haiku")
	// TierUnknown floor → no clamping
	if got != "haiku" {
		t.Errorf("unknown floor: got %q, want haiku", got)
	}
}
