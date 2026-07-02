package routing

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()

	routingYAML := `
subagents:
  defaults:
    model: sonnet
    categories:
      research: haiku
      review: sonnet
  phases:
    plan:
      model: opus
      categories:
        research: sonnet
    execute:
      model: sonnet
  overrides:
    "interflux:review:fd-safety": opus
dispatch:
  tiers:
    fast:
      model: haiku
      description: "Quick tasks"
    deep:
      model: opus
      description: "Complex tasks"
  fallback:
    medium: fast
complexity:
  mode: shadow
  tiers:
    simple:
      description: "Simple tasks"
      prompt_tokens: 1000
      file_count: 3
      reasoning_depth: 1
  overrides:
    complex:
      subagent_model: opus
      dispatch_tier: deep
`
	routingPath := filepath.Join(dir, "routing.yaml")
	if err := os.WriteFile(routingPath, []byte(routingYAML), 0644); err != nil {
		t.Fatal(err)
	}

	rolesYAML := `
roles:
  reviewer:
    description: "Code reviewers"
    model_tier: sonnet
    min_model: sonnet
    agents:
      - fd-safety
      - fd-correctness
  planner:
    description: "Planning agents"
    model_tier: opus
    min_model: sonnet
    agents:
      - fd-architecture
`
	rolesPath := filepath.Join(dir, "agent-roles.yaml")
	if err := os.WriteFile(rolesPath, []byte(rolesYAML), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(routingPath, rolesPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	// Subagent defaults
	if cfg.Subagents.Defaults.Model != "sonnet" {
		t.Errorf("defaults.model = %q, want sonnet", cfg.Subagents.Defaults.Model)
	}
	if v := cfg.Subagents.Defaults.Categories["research"]; v != "haiku" {
		t.Errorf("defaults.categories.research = %q, want haiku", v)
	}

	// Phase config
	plan, ok := cfg.Subagents.Phases["plan"]
	if !ok {
		t.Fatal("missing phase: plan")
	}
	if plan.Model != "opus" {
		t.Errorf("phases.plan.model = %q, want opus", plan.Model)
	}
	if v := plan.Categories["research"]; v != "sonnet" {
		t.Errorf("phases.plan.categories.research = %q, want sonnet", v)
	}

	// Overrides
	if v := cfg.Subagents.Overrides["interflux:review:fd-safety"]; v != "opus" {
		t.Errorf("overrides[fd-safety] = %q, want opus", v)
	}

	// Dispatch
	fast, ok := cfg.Dispatch.Tiers["fast"]
	if !ok {
		t.Fatal("missing tier: fast")
	}
	if fast.Model != "haiku" {
		t.Errorf("dispatch.tiers.fast.model = %q, want haiku", fast.Model)
	}
	if v := cfg.Dispatch.Fallback["medium"]; v != "fast" {
		t.Errorf("dispatch.fallback.medium = %q, want fast", v)
	}

	// Complexity
	if cfg.Complexity.Mode != "shadow" {
		t.Errorf("complexity.mode = %q, want shadow", cfg.Complexity.Mode)
	}

	// Roles/safety floors
	floors := cfg.SafetyFloors()
	if v := floors["fd-safety"]; v != "sonnet" {
		t.Errorf("floor[fd-safety] = %q, want sonnet", v)
	}
	if v := floors["fd-architecture"]; v != "sonnet" {
		t.Errorf("floor[fd-architecture] = %q, want sonnet", v)
	}
}

func TestLoadConfigMissingFile(t *testing.T) {
	_, err := LoadConfig("/nonexistent/routing.yaml", "")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoadConfigNoRoles(t *testing.T) {
	dir := t.TempDir()
	routingYAML := `
subagents:
  defaults:
    model: sonnet
`
	routingPath := filepath.Join(dir, "routing.yaml")
	if err := os.WriteFile(routingPath, []byte(routingYAML), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(routingPath, "")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Subagents.Defaults.Model != "sonnet" {
		t.Errorf("defaults.model = %q, want sonnet", cfg.Subagents.Defaults.Model)
	}
	// Nil maps should be initialized
	if cfg.Subagents.Phases == nil {
		t.Error("Phases map should be initialized")
	}
	if cfg.Subagents.Overrides == nil {
		t.Error("Overrides map should be initialized")
	}
}

func TestLoadConfigDefaultComplexityMode(t *testing.T) {
	dir := t.TempDir()
	routingYAML := `
subagents:
  defaults:
    model: haiku
`
	routingPath := filepath.Join(dir, "routing.yaml")
	if err := os.WriteFile(routingPath, []byte(routingYAML), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(routingPath, "")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Complexity.Mode != "off" {
		t.Errorf("complexity.mode = %q, want off (default)", cfg.Complexity.Mode)
	}
}

func TestSafetyFloorsEmpty(t *testing.T) {
	cfg := &Config{}
	floors := cfg.SafetyFloors()
	if len(floors) != 0 {
		t.Errorf("expected empty floors, got %d", len(floors))
	}
}

func TestSafetyFloorsSkipsEmptyMinModel(t *testing.T) {
	cfg := &Config{
		Roles: RolesConfig{
			Roles: map[string]RoleEntry{
				"editor": {
					MinModel: "",
					Agents:   []string{"fd-quality"},
				},
			},
		},
	}
	floors := cfg.SafetyFloors()
	if _, ok := floors["fd-quality"]; ok {
		t.Error("should not have floor for agent with empty min_model")
	}
}
