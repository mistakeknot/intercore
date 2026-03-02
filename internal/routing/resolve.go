package routing

import "strings"

// Resolver performs model resolution using loaded config.
type Resolver struct {
	cfg    *Config
	floors map[string]string // agent short name → min model
}

// NewResolver creates a resolver from config.
func NewResolver(cfg *Config) *Resolver {
	return &Resolver{
		cfg:    cfg,
		floors: cfg.SafetyFloors(),
	}
}

// Config returns the underlying configuration.
func (r *Resolver) Config() *Config { return r.cfg }

// ResolveOpts specifies the resolution context.
type ResolveOpts struct {
	Phase    string
	Category string
	Agent    string
}

// ResolveModel resolves the model for a given context.
// Resolution order (highest priority first):
//
//	overrides[agent] > phases[phase].categories[cat] > phases[phase].model >
//	defaults.categories[cat] > defaults.model > "sonnet"
//
// Then applies safety floor clamping.
func (r *Resolver) ResolveModel(opts ResolveOpts) string {
	result := ""

	// 1. Per-agent override
	if opts.Agent != "" {
		if v, ok := r.cfg.Subagents.Overrides[opts.Agent]; ok && v != "inherit" {
			result = v
		}
	}

	// 2. Phase-specific category
	if result == "" && opts.Phase != "" && opts.Category != "" {
		if phase, ok := r.cfg.Subagents.Phases[opts.Phase]; ok {
			if v, ok := phase.Categories[opts.Category]; ok && v != "inherit" {
				result = v
			}
		}
	}

	// 3. Phase-level model
	if result == "" && opts.Phase != "" {
		if phase, ok := r.cfg.Subagents.Phases[opts.Phase]; ok {
			if phase.Model != "" && phase.Model != "inherit" {
				result = phase.Model
			}
		}
	}

	// 4. Default category
	if result == "" && opts.Category != "" {
		if v, ok := r.cfg.Subagents.Defaults.Categories[opts.Category]; ok && v != "inherit" {
			result = v
		}
	}

	// 5. Default model
	if result == "" && r.cfg.Subagents.Defaults.Model != "" && r.cfg.Subagents.Defaults.Model != "inherit" {
		result = r.cfg.Subagents.Defaults.Model
	}

	// 6. Ultimate fallback
	if result == "" || result == "inherit" {
		result = "sonnet"
	}

	// Safety floor clamping
	if opts.Agent != "" {
		result = r.applyFloor(opts.Agent, result)
	}

	return result
}

// ResolveDispatchTier resolves a dispatch tier name to a model ID.
// Follows the fallback chain up to 3 hops.
func (r *Resolver) ResolveDispatchTier(tier string) string {
	for hops := 0; hops < 3; hops++ {
		if t, ok := r.cfg.Dispatch.Tiers[tier]; ok {
			return t.Model
		}
		if fb, ok := r.cfg.Dispatch.Fallback[tier]; ok {
			tier = fb
		} else {
			break
		}
	}
	return ""
}

// ResolveBatch resolves models for a list of agent short names.
// Returns map[agentShortName]model. Infers category from agent name patterns.
func (r *Resolver) ResolveBatch(agents []string, phase string) map[string]string {
	result := make(map[string]string, len(agents))
	for _, agent := range agents {
		category := inferCategory(agent)
		model := r.ResolveModel(ResolveOpts{
			Phase:    phase,
			Category: category,
			Agent:    inferAgentID(agent),
		})
		result[agent] = model
	}
	return result
}

// applyFloor clamps model up to the safety floor if one exists.
// Handles namespaced agent IDs by stripping to short name.
func (r *Resolver) applyFloor(agent, model string) string {
	// Try full agent ID first
	floor, ok := r.floors[agent]
	if !ok && strings.Contains(agent, ":") {
		// Strip namespace: "interflux:review:fd-safety" → "fd-safety"
		parts := strings.Split(agent, ":")
		short := parts[len(parts)-1]
		floor, ok = r.floors[short]
	}
	if !ok {
		return model
	}

	modelTier := ParseModelTier(model)
	floorTier := ParseModelTier(floor)
	if floorTier == TierUnknown || modelTier >= floorTier {
		return model
	}
	return floor
}

// InferCategoryExported is the exported version of inferCategory for CLI use.
func InferCategoryExported(agent string) string {
	return inferCategory(agent)
}

// inferCategory determines routing category from agent name patterns.
func inferCategory(agent string) string {
	if strings.HasSuffix(agent, "-researcher") || agent == "repo-research-analyst" {
		return "research"
	}
	if strings.HasPrefix(agent, "fd-") {
		return "review"
	}
	return ""
}

// inferAgentID maps short agent names to namespaced IDs for override lookup.
func inferAgentID(agent string) string {
	if strings.HasSuffix(agent, "-researcher") || agent == "repo-research-analyst" {
		return "interflux:research:" + agent
	}
	if strings.HasPrefix(agent, "fd-") {
		return "interflux:review:" + agent
	}
	return agent
}
