package routing

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func effortConfig() *Config {
	return &Config{
		Reasoning: ReasoningPolicy{
			FrontierModels:  []string{"gpt-6-astra", "claude-opus-5"},
			FrontierReasons: []string{"foundational-invariants", "difficult-verification", "capability-failure", "broad-consequences"},
		},
		Dispatch: DispatchConfig{
			EffortFloors: map[string]string{"foundational-invariants": "high", "difficult-verification": "high", "capability-failure": "high"},
			Roles:        map[string]string{"planning": "astra", "frontier-planning": "astra", "deep-execution": "astra", "validation": "astra"},
			Tiers: map[string]DispatchProfile{
				"astra": {Backend: "codex", Model: "gpt-6-astra", ReasoningEffort: "medium", Fallbacks: []string{"opus", "sol"}},
				"opus":  {Backend: "claude", Model: "claude-opus-5", ReasoningEffort: "medium", Fallbacks: []string{"sol"}},
				"sol":   {Backend: "codex", Model: "gpt-6-sol", ReasoningEffort: "medium", Fallbacks: []string{"kimi"}},
				"kimi":  {Backend: "kimi", Model: "kimi-code/k3", ReasoningEffort: "high"},
			},
		},
	}
}

func effortContext(reasons ...string) DecisionContext {
	return DecisionContext{Reasons: reasons, Rationale: "Specified effort contract",
		Handoff: &ReasoningHandoff{Decisions: "settled", Constraints: "bounded", Verification: "tests", Escalation: "review"}}
}

func setEffort(cfg *Config, ref, effort string) {
	p := cfg.Dispatch.Tiers[ref]
	p.ReasoningEffort = effort
	cfg.Dispatch.Tiers[ref] = p
}

func requireFloorEntries(t *testing.T, got ReasoningDecision, want ...EffortFloorApplication) {
	t.Helper()
	if !reflect.DeepEqual(got.EffortFloorsApplied, want) {
		t.Fatalf("floors = %+v, want %+v", got.EffortFloorsApplied, want)
	}
}

func TestReasoningEffortFloorsRaiseFoundationalPlanning(t *testing.T) {
	d, err := NewResolver(effortConfig()).ResolveDecision("planning", "", "", effortContext("foundational-invariants"))
	if err != nil {
		t.Fatal(err)
	}
	if d.ProfileRef != "astra" || d.Profile.ReasoningEffort != "high" || d.FallbackReason != "" {
		t.Fatalf("decision = %+v", d)
	}
	requireFloorEntries(t, d,
		EffortFloorApplication{"astra", []string{"foundational-invariants"}, "medium", "high"},
		EffortFloorApplication{"opus", []string{"foundational-invariants"}, "medium", "high"},
		EffortFloorApplication{"sol", []string{"foundational-invariants"}, "medium", "high"})
}

func TestReasoningEffortFloorsCoverExpandedFallbacks(t *testing.T) {
	cfg := effortConfig()
	setEffort(cfg, "sol", "xhigh")
	d, err := NewResolver(cfg).ResolveDecision("deep-execution", "", "", effortContext("foundational-invariants"))
	if err != nil {
		t.Fatal(err)
	}
	if d.FrontierRequired || !slices.Equal(refsOf(d.ResolvedDispatch), []string{"astra", "opus", "sol", "kimi"}) {
		t.Fatalf("candidates = %+v", d)
	}
	if d.Profile.ReasoningEffort != "high" || d.FallbackChain[0].Profile.ReasoningEffort != "high" || d.FallbackChain[1].Profile.ReasoningEffort != "xhigh" || d.FallbackChain[2].Profile.ReasoningEffort != "high" {
		t.Fatalf("efforts = %+v", d)
	}
	requireFloorEntries(t, d,
		EffortFloorApplication{"astra", []string{"foundational-invariants"}, "medium", "high"},
		EffortFloorApplication{"opus", []string{"foundational-invariants"}, "medium", "high"})
}

func TestReasoningEffortFloorsAuditExcludedCandidates(t *testing.T) {
	for _, ordinary := range []string{"frontier_required", "model_unavailable"} {
		t.Run(ordinary, func(t *testing.T) {
			c := effortContext("foundational-invariants")
			role, excluded := "planning", "sol"
			if ordinary == "model_unavailable" {
				role, excluded = "deep-execution", "astra"
				c.AvailableModels = []string{"claude-opus-5"}
			}
			d, err := NewResolver(effortConfig()).ResolveDecision(role, "", "", c)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, e := range d.Excluded {
				if e.ProfileRef == excluded {
					found = true
					if e.Reason != ordinary || e.Profile.ReasoningEffort != "high" {
						t.Fatalf("excluded = %+v", e)
					}
				}
			}
			if !found || !slices.ContainsFunc(d.EffortFloorsApplied, func(a EffortFloorApplication) bool {
				return a.ProfileRef == excluded && a.From == "medium" && a.To == "high"
			}) {
				t.Fatalf("missing audit: %+v", d)
			}
		})
	}
}

func TestReasoningEffortFloorsFailClosed(t *testing.T) {
	for _, tc := range []struct{ ref, backend, effort, floor string }{
		{"astra", "unknown", "medium", "high"}, {"astra", "main", "medium", "high"},
		{"astra", "codex", "extreme", "high"}, {"opus", "claude", "medium", "xhigh"}, {"kimi", "kimi", "high", "xhigh"},
	} {
		for _, filter := range []string{"eligible", "frontier", "unavailable"} {
			t.Run(tc.ref+"/"+tc.backend+"/"+tc.effort+"/"+filter, func(t *testing.T) {
				cfg := effortConfig()
				cfg.Dispatch.EffortFloors["foundational-invariants"] = tc.floor
				p := cfg.Dispatch.Tiers[tc.ref]
				p.Backend, p.ReasoningEffort = tc.backend, tc.effort
				cfg.Dispatch.Tiers[tc.ref] = p
				c := effortContext("foundational-invariants")
				role := "deep-execution"
				if filter == "frontier" {
					role = "planning"
					cfg.Reasoning.FrontierModels = []string{"gpt-6-sol"}
				}
				if filter == "unavailable" {
					c.AvailableModels = []string{"gpt-6-sol"}
				}
				d, err := NewResolver(cfg).ResolveDecision(role, "", "", c)
				if err != nil {
					t.Fatal(err)
				}
				if !slices.ContainsFunc(d.Excluded, func(e ExcludedDispatchCandidate) bool {
					return e.ProfileRef == tc.ref && e.Reason == "unsupported_adapter" && e.Profile.ReasoningEffort == tc.effort
				}) {
					t.Fatalf("missing exclusion: %+v", d)
				}
				if slices.ContainsFunc(d.EffortFloorsApplied, func(a EffortFloorApplication) bool { return a.ProfileRef == tc.ref }) {
					t.Fatalf("partial raise audited: %+v", d)
				}
			})
		}
	}
}

func TestReasoningEffortFloorsAllCandidatesExcluded(t *testing.T) {
	cfg := effortConfig()
	cfg.Dispatch.EffortFloors["foundational-invariants"] = "xhigh"
	c := effortContext("foundational-invariants")
	c.AvailableModels = []string{"claude-opus-5", "kimi-code/k3"}
	d, err := NewResolver(cfg).ResolveDecision("deep-execution", "", "", c)
	if err == nil || !strings.Contains(err.Error(), "no eligible model") {
		t.Fatalf("err = %v", err)
	}
	if d.ProfileRef != "" || !reflect.DeepEqual(d.Profile, DispatchProfile{}) || len(d.FallbackChain) != 0 || len(d.Excluded) != 4 || len(d.EffortFloorsApplied) != 2 || len(d.ClassificationReasons) != 1 {
		t.Fatalf("partial decision = %+v", d)
	}
}

func TestReasoningEffortFloorsKimiHighIsNoop(t *testing.T) {
	c := effortContext("foundational-invariants")
	c.AvailableModels = []string{"kimi-code/k3"}
	cfg := effortConfig()
	cfg.Dispatch.Roles["validation"] = "kimi"
	d, err := NewResolver(cfg).ResolveDecision("validation", "gpt-6-astra", "", c)
	if err != nil || d.ProfileRef != "kimi" || d.Profile.ReasoningEffort != "high" || d.FrontierRequired {
		t.Fatalf("decision = %+v, %v", d, err)
	}
	requireFloorEntries(t, d)
}

func TestReasoningEffortFloorsReasonsAndNoops(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		floors               map[string]string
		reasons, wantReasons []string
		want                 string
	}{
		{"strongest", map[string]string{"foundational-invariants": "high", "difficult-verification": "xhigh", "capability-failure": "high", "broad-consequences": "medium"}, []string{"foundational-invariants", "capability-failure", "difficult-verification", "foundational-invariants", "broad-consequences"}, []string{"capability-failure", "difficult-verification", "foundational-invariants"}, "xhigh"},
		{"unmatched", map[string]string{"foundational-invariants": "high"}, []string{"broad-consequences"}, nil, "medium"},
		{"absent", nil, []string{"foundational-invariants"}, nil, "medium"},
		{"empty", map[string]string{}, []string{"foundational-invariants"}, nil, "medium"},
		{"equal-and-lower", map[string]string{"foundational-invariants": "medium", "capability-failure": "low"}, []string{"foundational-invariants", "capability-failure"}, nil, "medium"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := effortConfig()
			cfg.Dispatch.EffortFloors = tc.floors
			p := cfg.Dispatch.Tiers["astra"]
			p.Fallbacks = nil
			cfg.Dispatch.Tiers["astra"] = p
			d, err := NewResolver(cfg).ResolveDecision("planning", "", "", effortContext(tc.reasons...))
			if err != nil || d.Profile.ReasoningEffort != tc.want {
				t.Fatalf("decision = %+v, %v", d, err)
			}
			if tc.wantReasons == nil {
				requireFloorEntries(t, d)
				raw, err := json.Marshal(d)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(raw), "effort_floors_applied") {
					t.Fatalf("no-op serialized: %s", raw)
				}
			} else {
				requireFloorEntries(t, d, EffortFloorApplication{"astra", tc.wantReasons, "medium", tc.want})
			}
		})
	}
}

func TestReasoningEffortFloorsValidateProgrammaticConfig(t *testing.T) {
	for _, floors := range []map[string]string{{"typo": "high"}, {"foundational-invariants": ""}, {"foundational-invariants": "extreme"}} {
		cfg := effortConfig()
		cfg.Dispatch.EffortFloors = floors
		_, err := NewResolver(cfg).ResolveDecision("planning", "", "", DecisionContext{})
		if err == nil || !strings.Contains(err.Error(), "dispatch.effort_floors") {
			t.Fatalf("err = %v", err)
		}
	}
}

func TestReasoningEffortFloorsPreserveMalformedProfiles(t *testing.T) {
	for _, field := range []string{"backend", "model", "effort"} {
		for _, excluded := range []bool{false, true} {
			t.Run(field+"/"+map[bool]string{false: "eligible", true: "excluded"}[excluded], func(t *testing.T) {
				cfg := effortConfig()
				p := cfg.Dispatch.Tiers["astra"]
				switch field {
				case "backend":
					p.Backend = ""
				case "model":
					p.Model = ""
				case "effort":
					p.ReasoningEffort = ""
				}
				cfg.Dispatch.Tiers["astra"] = p
				c := effortContext("foundational-invariants")
				if excluded {
					c.AvailableModels = []string{"claude-opus-5"}
				}
				d, err := NewResolver(cfg).ResolveDecision("deep-execution", "", "", c)
				if !excluded {
					if err == nil || !strings.Contains(err.Error(), "incomplete profile") {
						t.Fatalf("err = %v", err)
					}
					return
				}
				if err != nil || d.Excluded[0].Reason != "model_unavailable" {
					t.Fatalf("decision = %+v, %v", d, err)
				}
			})
		}
	}
}

func TestReasoningEffortExclusionPreservesFallbackAttribution(t *testing.T) {
	for _, tc := range []struct {
		name, bad, producer, want, reason string
		otherLab                          bool
	}{
		{"promoted-primary", "sol", "claude-fable-5-1", "opus", "reasoning_contract", false},
		{"policy-primary", "opus", "claude-fable-5-1", "sol", "reasoning_contract", false},
		{"surviving-reorder", "opus", "claude-fable-5-1", "sol", "reasoning_contract", true},
		{"producer-conflict", "sol", "claude-opus-5", "sonnet", "producer_model_conflict", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := crossLabConfigOpusFallsBackToSolFirst()
			cfg.Reasoning.FrontierReasons = []string{"foundational-invariants"}
			cfg.Dispatch.EffortFloors = map[string]string{"foundational-invariants": "high"}
			if tc.producer == "claude-opus-5" {
				setEffort(cfg, "opus", "medium")
			}
			if tc.otherLab {
				cfg.Reasoning.FrontierModels = append(cfg.Reasoning.FrontierModels, "kimi-code/k3")
			}
			p := cfg.Dispatch.Tiers[tc.bad]
			p.Backend = "main"
			cfg.Dispatch.Tiers[tc.bad] = p
			d, err := NewResolver(cfg).ResolveDecision("validation", tc.producer, "", effortContext("foundational-invariants"))
			if err != nil || d.ProfileRef != tc.want || d.FallbackReason != tc.reason {
				t.Fatalf("decision = %+v, %v", d, err)
			}
			if (d.CrossLabReorder != nil) != tc.otherLab {
				t.Fatalf("reorder = %+v", d.CrossLabReorder)
			}
			if tc.producer == "claude-opus-5" && (d.Excluded[0].Reason != "producer_model_conflict" || d.Excluded[0].Profile.ReasoningEffort != "medium" || slices.ContainsFunc(d.EffortFloorsApplied, func(a EffortFloorApplication) bool { return a.ProfileRef == "opus" })) {
				t.Fatalf("upstream conflict changed: %+v", d.Excluded)
			}
		})
	}
}

func TestReasoningEffortResolutionDoesNotMutateConfig(t *testing.T) {
	cfg := effortConfig()
	before, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := NewResolver(cfg)
	c := effortContext("foundational-invariants")
	override := "low"
	c.EffortOverride = &override
	d, err := r.ResolveDecision("deep-execution", "", "", c)
	if err != nil || d.Profile.ReasoningEffort != "high" {
		t.Fatalf("decision = %+v, %v", d, err)
	}
	after, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("configuration mutated:\nbefore %s\nafter %s", before, after)
	}
	d, err = r.ResolveDecision("deep-execution", "", "", DecisionContext{})
	if err != nil || d.Profile.ReasoningEffort != "medium" || d.FallbackChain[0].Profile.ReasoningEffort != "medium" || d.EffortOverride != nil {
		t.Fatalf("subsequent resolution = %+v, %v", d, err)
	}
	requireFloorEntries(t, d)
}

func TestReasoningEffortOverrideBeforeFloors(t *testing.T) {
	cfg := effortConfig()
	cfg.Dispatch.Roles["frontier-planning"] = "opus"
	setEffort(cfg, "opus", "high")
	c := effortContext("foundational-invariants")
	override := "medium"
	c.EffortOverride = &override
	d, err := NewResolver(cfg).ResolveDecision("planning", "", "", c)
	if err != nil {
		t.Fatal(err)
	}
	final := "high"
	want := &EffortOverrideApplication{ProfileRef: "opus", Requested: "medium", Applied: true, Changed: true, From: "high", To: "medium", FinalEffort: &final}
	if !reflect.DeepEqual(d.EffortOverride, want) || d.Profile.ReasoningEffort != "high" {
		t.Fatalf("override = %+v, profile = %+v", d.EffortOverride, d.Profile)
	}
	requireFloorEntries(t, d, EffortFloorApplication{"opus", []string{"foundational-invariants"}, "medium", "high"}, EffortFloorApplication{"sol", []string{"foundational-invariants"}, "medium", "high"})
}

func TestReasoningEffortOverrideCannotLowerHigherEffortViaFloor(t *testing.T) {
	c := effortContext("foundational-invariants")
	override := "xhigh"
	c.EffortOverride = &override
	d, err := NewResolver(effortConfig()).ResolveDecision("planning", "", "", c)
	if err != nil || d.Profile.ReasoningEffort != "xhigh" || d.EffortOverride == nil || d.EffortOverride.FinalEffort == nil || *d.EffortOverride.FinalEffort != "xhigh" {
		t.Fatalf("decision = %+v, %v", d, err)
	}
	if slices.ContainsFunc(d.EffortFloorsApplied, func(a EffortFloorApplication) bool { return a.ProfileRef == "astra" }) {
		t.Fatal("floor lowered xhigh")
	}
}

func TestReasoningEffortOverrideNoopRecorded(t *testing.T) {
	c := DecisionContext{}
	override := "medium"
	c.EffortOverride = &override
	d, err := NewResolver(effortConfig()).ResolveDecision("planning", "", "", c)
	if err != nil {
		t.Fatal(err)
	}
	want := &EffortOverrideApplication{ProfileRef: "astra", Requested: "medium", Applied: true, Changed: false, From: "medium", To: "medium", FinalEffort: &override}
	if !reflect.DeepEqual(d.EffortOverride, want) {
		t.Fatalf("override = %+v", d.EffortOverride)
	}
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"changed":false`) || !strings.Contains(string(raw), `"applied":true`) {
		t.Fatalf("receipt = %s", raw)
	}
}

func TestReasoningEffortOverrideTargetsResolvedPrimaryOnly(t *testing.T) {
	for _, filter := range []string{"eligible", "unavailable", "frontier", "producer-filtered"} {
		t.Run(filter, func(t *testing.T) {
			cfg := crossLabConfig()
			cfg.Reasoning.FrontierReasons = []string{"foundational-invariants"}
			c := DecisionContext{}
			override := "medium"
			c.EffortOverride = &override
			role, producer, selected := "validation", "claude-fable-5-1", "sol"
			if filter == "unavailable" {
				c.AvailableModels = []string{"claude-opus-5", "claude-sonnet-5", "kimi-code/k3"}
				selected = "opus"
			}
			if filter == "frontier" {
				role = "planning"
				cfg.Dispatch.Roles["frontier-planning"] = "sol"
				p := cfg.Dispatch.Tiers["sol"]
				p.Fallbacks = []string{"opus"}
				cfg.Dispatch.Tiers["sol"] = p
				c.Reasons = []string{"foundational-invariants"}
				c.Rationale = "reviewed"
				selected = "opus"
			}
			if filter == "producer-filtered" {
				producer = "claude-opus-5"
			}
			d, err := NewResolver(cfg).ResolveDecision(role, producer, "", c)
			if err != nil {
				t.Fatal(err)
			}
			if d.ProfileRef != selected || d.EffortOverride == nil || d.EffortOverride.ProfileRef != "sol" || d.EffortOverride.FinalEffort == nil || *d.EffortOverride.FinalEffort != "medium" {
				t.Fatalf("target = %+v; decision = %+v", d.EffortOverride, d)
			}
			candidates := append([]DispatchCandidate{{ProfileRef: d.ProfileRef, Profile: d.Profile}}, d.FallbackChain...)
			for _, e := range d.Excluded {
				candidates = append(candidates, e.DispatchCandidate)
			}
			for _, p := range candidates {
				want := "high"
				if p.ProfileRef == "sol" {
					want = "medium"
				}
				if p.Profile.ReasoningEffort != want {
					t.Fatalf("retargeted effort: %+v", p)
				}
			}
		})
	}
}

func TestReasoningEffortOverrideUnsupported(t *testing.T) {
	for _, value := range []string{"", "extreme", "HIGH"} {
		t.Run("invalid/"+value, func(t *testing.T) {
			c := DecisionContext{EffortOverride: &value}
			_, err := NewResolver(effortConfig()).ResolveDecision("planning", "", "", c)
			if err == nil || !strings.Contains(err.Error(), "effort_override") {
				t.Fatalf("err = %v", err)
			}
		})
	}
	for _, tc := range []struct{ ref, backend, override string }{
		{"opus", "claude", "xhigh"}, {"opus", "claude", "low"},
		{"kimi", "kimi", "medium"}, {"astra", "main", "high"}, {"astra", "unknown", "high"},
	} {
		t.Run(tc.ref+"/"+tc.backend+"/"+tc.override, func(t *testing.T) {
			cfg := effortConfig()
			cfg.Dispatch.Roles["deep-execution"] = tc.ref
			p := cfg.Dispatch.Tiers[tc.ref]
			p.Backend = tc.backend
			p.Fallbacks = []string{"sol"}
			cfg.Dispatch.Tiers[tc.ref] = p
			c := effortContext("foundational-invariants")
			c.EffortOverride = &tc.override
			d, err := NewResolver(cfg).ResolveDecision("deep-execution", "", "", c)
			if err != nil {
				t.Fatal(err)
			}
			if d.ProfileRef != "sol" || d.Excluded[0].ProfileRef != tc.ref || d.Excluded[0].Reason != "unsupported_adapter" {
				t.Fatalf("decision = %+v", d)
			}
			want := &EffortOverrideApplication{ProfileRef: tc.ref, Requested: tc.override, From: p.ReasoningEffort, To: p.ReasoningEffort}
			if !reflect.DeepEqual(d.EffortOverride, want) {
				t.Fatalf("override = %+v, want %+v", d.EffortOverride, want)
			}
			raw, err := json.Marshal(d.EffortOverride)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(raw), `"final_effort":null`) {
				t.Fatalf("failure final = %s", raw)
			}
		})
	}
	t.Run("floor-fails-after-supported-override", func(t *testing.T) {
		cfg := effortConfig()
		cfg.Dispatch.Roles["deep-execution"] = "opus"
		cfg.Dispatch.EffortFloors["foundational-invariants"] = "xhigh"
		c := effortContext("foundational-invariants")
		override := "high"
		c.EffortOverride = &override
		d, err := NewResolver(cfg).ResolveDecision("deep-execution", "", "", c)
		if err != nil || d.EffortOverride == nil || !d.EffortOverride.Applied || d.EffortOverride.FinalEffort != nil || d.Excluded[0].Profile.ReasoningEffort != "high" {
			t.Fatalf("decision = %+v, %v", d, err)
		}
		if slices.ContainsFunc(d.EffortFloorsApplied, func(a EffortFloorApplication) bool { return a.ProfileRef == "opus" }) {
			t.Fatal("partial floor receipt")
		}
	})
}

func TestReasoningEffortOverrideDoesNotRepairIncompleteProfile(t *testing.T) {
	for _, excluded := range []bool{false, true} {
		cfg := effortConfig()
		setEffort(cfg, "astra", "")
		override := "high"
		c := DecisionContext{EffortOverride: &override}
		if excluded {
			c.AvailableModels = []string{"claude-opus-5"}
		}
		d, err := NewResolver(cfg).ResolveDecision("planning", "", "", c)
		if !excluded {
			if err == nil || !strings.Contains(err.Error(), "incomplete profile") {
				t.Fatalf("err = %v", err)
			}
		} else if err != nil || d.Excluded[0].Reason != "model_unavailable" {
			t.Fatalf("decision = %+v, %v", d, err)
		}
		if d.EffortOverride == nil || d.EffortOverride.ProfileRef != "astra" || d.EffortOverride.Applied || d.EffortOverride.FinalEffort != nil {
			t.Fatalf("incomplete override = %+v", d.EffortOverride)
		}
	}
}

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
    sol: {model: gpt-6-sol, backend: codex, reasoning_effort: high}
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
	c.AvailableModels = []string{"gpt-6-sol"}
	if _, err := r.ResolveDecision("planning", "", "", c); err == nil {
		t.Fatal("silently downgraded without frontier access")
	}
}

func TestReasoningJudgmentAndHandoff(t *testing.T) {
	r := NewResolver(reasoningConfig(t))
	got, err := r.ResolveDecision("planning", "", "", DecisionContext{Domain: "games"})
	if err != nil || got.Profile.Model != "gpt-6-sol" {
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
	if err != nil || got.Profile.Model != "gpt-6-sol" {
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
				"sol":    {Backend: "codex", Model: "gpt-6-sol", ReasoningEffort: "high"},
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
	c := DecisionContext{AvailableModels: []string{"gpt-6-sol", "claude-sonnet-5", "kimi-code/k3", "claude-fable-5-1"}}
	got, err := NewResolver(crossLabConfigOpusFallsBackToSolFirst()).ResolveDecision("validation", "claude-fable-5-1", "", c)
	if err != nil {
		t.Fatal(err)
	}
	if got.Profile.Model != "gpt-6-sol" {
		t.Fatalf("selected %q, want gpt-6-sol", got.Profile.Model)
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
	c := DecisionContext{AvailableModels: []string{"gpt-6-sol", "claude-sonnet-5", "kimi-code/k3", "claude-fable-5-1"}}
	got, err := NewResolver(cfg).ResolveDecision("validation", "claude-fable-5-1", "", c)
	if err != nil {
		t.Fatal(err)
	}
	if got.Profile.Model != "gpt-6-sol" {
		t.Fatalf("selected %q, want gpt-6-sol", got.Profile.Model)
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
