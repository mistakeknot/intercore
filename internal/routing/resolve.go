package routing

import (
	"os"
	"strings"
)

// DispatchCandidate is a named fallback profile returned to dispatchers.
type DispatchCandidate struct {
	ProfileRef string          `json:"profile_ref"`
	Profile    DispatchProfile `json:"profile"`
}

// ResolvedDispatch is the complete role/tier resolution contract. The primary
// profile is separate from the ordered fallback chain so existing callers can
// continue consuming only Profile.Model.
type ResolvedDispatch struct {
	RequestedRole string              `json:"requested_role,omitempty"`
	RequestedTier string              `json:"requested_tier,omitempty"`
	ProfileRef    string              `json:"profile_ref"`
	Profile       DispatchProfile     `json:"profile"`
	FallbackChain []DispatchCandidate `json:"fallback_chain"`
}

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

	// Fable-window fallback: fable resolves only while the window is open;
	// otherwise degrade to opus (fail-closed, never below today's tier).
	if result == "fable" && !fableWindowOpen() {
		result = "opus"
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
	resolved, ok := r.resolveDispatch(tier)
	if !ok {
		return ""
	}
	return resolved.Profile.Model
}

// ResolveDispatchRole resolves a named role to a complete executable profile
// and its ordered fallback chain.
func (r *Resolver) ResolveDispatchRole(role string) (ResolvedDispatch, bool) {
	profileRef, ok := r.cfg.Dispatch.Roles[role]
	if !ok || profileRef == "" {
		return ResolvedDispatch{}, false
	}
	resolved, ok := r.resolveDispatch(profileRef)
	if !ok {
		return ResolvedDispatch{}, false
	}
	resolved.RequestedRole = role
	if resolved.Profile.Role == "" {
		resolved.Profile.Role = role
	}
	return resolved, true
}

// ResolveDispatchProfile resolves a tier/profile reference and returns the
// primary profile plus all reachable fallbacks in deterministic declaration
// order. Cycles and duplicate references are ignored.
func (r *Resolver) ResolveDispatchProfile(profileRef string) (ResolvedDispatch, bool) {
	resolved, ok := r.resolveDispatch(profileRef)
	if ok {
		resolved.RequestedTier = profileRef
	}
	return resolved, ok
}

func (r *Resolver) resolveDispatch(profileRef string) (ResolvedDispatch, bool) {
	primaryRef, primary, ok := r.lookupDispatchProfile(profileRef)
	if !ok {
		return ResolvedDispatch{}, false
	}

	resolved := ResolvedDispatch{
		ProfileRef:    primaryRef,
		Profile:       primary,
		FallbackChain: []DispatchCandidate{},
	}
	seen := map[string]bool{primaryRef: true}
	var appendFallbacks func([]string)
	appendFallbacks = func(refs []string) {
		for _, ref := range refs {
			actualRef, profile, found := r.lookupDispatchProfile(ref)
			if !found || seen[actualRef] {
				continue
			}
			seen[actualRef] = true
			resolved.FallbackChain = append(resolved.FallbackChain, DispatchCandidate{
				ProfileRef: actualRef,
				Profile:    profile,
			})
			appendFallbacks(profile.Fallbacks)
		}
	}
	appendFallbacks(primary.Fallbacks)
	return resolved, true
}

// lookupDispatchProfile follows the legacy fallback alias map only while a
// concrete profile is absent. Concrete profile fallbacks use the ordered
// DispatchProfile.Fallbacks field instead.
func (r *Resolver) lookupDispatchProfile(profileRef string) (string, DispatchProfile, bool) {
	seen := map[string]bool{}
	for profileRef != "" && !seen[profileRef] {
		seen[profileRef] = true
		if profile, ok := r.cfg.Dispatch.Tiers[profileRef]; ok {
			return profileRef, profile, true
		}
		profileRef = r.cfg.Dispatch.Fallback[profileRef]
	}
	return "", DispatchProfile{}, false
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

// fableWindowOpen reports whether the frontier (fable) window is open.
// Fail-closed: only an explicit CLAVAIN_FABLE_AVAILABLE=1 opens it.
func fableWindowOpen() bool {
	return os.Getenv("CLAVAIN_FABLE_AVAILABLE") == "1"
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
