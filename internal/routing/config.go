package routing

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config is the unified routing configuration.
type Config struct {
	Subagents  SubagentConfig   `yaml:"subagents"`
	Dispatch   DispatchConfig   `yaml:"dispatch"`
	Complexity ComplexityConfig `yaml:"complexity"`
	Roles      RolesConfig      `yaml:"-"` // loaded from separate file
}

// SubagentConfig holds Claude Code subagent routing rules.
type SubagentConfig struct {
	Defaults  SubagentDefaults       `yaml:"defaults"`
	Phases    map[string]PhaseConfig `yaml:"phases"`
	Overrides map[string]string      `yaml:"overrides"`
}

// SubagentDefaults holds the base routing defaults.
type SubagentDefaults struct {
	Model      string            `yaml:"model"`
	Categories map[string]string `yaml:"categories"`
}

// PhaseConfig holds per-phase model routing.
type PhaseConfig struct {
	Model      string            `yaml:"model"`
	Categories map[string]string `yaml:"categories"`
}

// DispatchConfig holds Codex CLI dispatch tier routing.
type DispatchConfig struct {
	Tiers    map[string]TierConfig `yaml:"tiers"`
	Fallback map[string]string     `yaml:"fallback"`
}

// TierConfig holds a single dispatch tier definition.
type TierConfig struct {
	Model       string `yaml:"model"`
	Description string `yaml:"description"`
}

// ComplexityConfig holds B2 complexity-aware routing.
type ComplexityConfig struct {
	Mode      string                        `yaml:"mode"` // off, shadow, enforce
	Tiers     map[string]ComplexityTier     `yaml:"tiers"`
	Overrides map[string]ComplexityOverride `yaml:"overrides"`
}

// ComplexityTier defines thresholds for a complexity class.
type ComplexityTier struct {
	Description    string `yaml:"description"`
	PromptTokens   int    `yaml:"prompt_tokens"`
	FileCount      int    `yaml:"file_count"`
	ReasoningDepth int    `yaml:"reasoning_depth"`
}

// ComplexityOverride defines model overrides for a complexity class.
type ComplexityOverride struct {
	SubagentModel string `yaml:"subagent_model"`
	DispatchTier  string `yaml:"dispatch_tier"`
}

// RolesConfig holds agent-roles.yaml data (safety floors).
type RolesConfig struct {
	Roles map[string]RoleEntry `yaml:"roles"`
}

// RoleEntry defines a single role with its agents and floor.
type RoleEntry struct {
	Description string   `yaml:"description"`
	ModelTier   string   `yaml:"model_tier"`
	MinModel    string   `yaml:"min_model"`
	Agents      []string `yaml:"agents"`
}

// LoadConfig loads routing.yaml and optionally agent-roles.yaml.
func LoadConfig(routingPath, rolesPath string) (*Config, error) {
	cfg := &Config{}

	data, err := os.ReadFile(routingPath)
	if err != nil {
		return nil, fmt.Errorf("read routing.yaml: %w", err)
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse routing.yaml: %w", err)
	}

	// Initialize nil maps
	if cfg.Subagents.Phases == nil {
		cfg.Subagents.Phases = map[string]PhaseConfig{}
	}
	if cfg.Subagents.Overrides == nil {
		cfg.Subagents.Overrides = map[string]string{}
	}
	if cfg.Subagents.Defaults.Categories == nil {
		cfg.Subagents.Defaults.Categories = map[string]string{}
	}
	if cfg.Dispatch.Tiers == nil {
		cfg.Dispatch.Tiers = map[string]TierConfig{}
	}
	if cfg.Dispatch.Fallback == nil {
		cfg.Dispatch.Fallback = map[string]string{}
	}
	if cfg.Complexity.Mode == "" {
		cfg.Complexity.Mode = "off"
	}

	// Load agent-roles.yaml if path provided
	if rolesPath != "" {
		rolesData, err := os.ReadFile(rolesPath)
		if err == nil {
			var roles RolesConfig
			if err := yaml.Unmarshal(rolesData, &roles); err == nil {
				cfg.Roles = roles
			}
		}
		// Non-fatal: safety floors are a progressive enhancement
	}

	return cfg, nil
}

// SafetyFloors extracts agent → min_model mapping from roles config.
func (c *Config) SafetyFloors() map[string]string {
	floors := make(map[string]string)
	for _, role := range c.Roles.Roles {
		if role.MinModel == "" {
			continue
		}
		for _, agent := range role.Agents {
			floors[agent] = role.MinModel
		}
	}
	return floors
}
