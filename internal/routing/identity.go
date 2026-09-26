package routing

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// ExcludedDispatchCandidate preserves the immutable profile and exclusion rule.
type ExcludedDispatchCandidate struct {
	DispatchCandidate
	Reason string `json:"reason"`
}

var snapshotSuffix = regexp.MustCompile(`-(?:[0-9]{4}-[0-9]{2}-[0-9]{2}|[0-9]{8})$`)

func stripModelDecoration(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	for {
		previous := model
		for _, provider := range []string{"openai", "anthropic", "codex", "claude", "claude-code", "moonshot"} {
			model = strings.TrimPrefix(model, provider+"/")
			model = strings.TrimPrefix(model, provider+":")
		}
		if model == previous {
			break
		}
	}
	for _, suffix := range []string{"[1m]", "[200k]"} {
		model = strings.TrimSuffix(model, suffix)
	}
	return model
}

func (r *Resolver) expandModelAlias(model string) (string, error) {
	model = stripModelDecoration(model)
	seen := map[string]bool{}
	for {
		if seen[model] {
			return "", fmt.Errorf("cyclic dispatch model alias %q", model)
		}
		seen[model] = true
		next, ok := r.cfg.Dispatch.ModelAliases[model]
		if !ok {
			return model, nil
		}
		model = stripModelDecoration(next)
		if model == "" {
			return "", fmt.Errorf("empty dispatch model alias")
		}
	}
}

// CanonicalModelIdentity compares model families conservatively across provider
// wrappers, context-window decorations, configured aliases and dated snapshots.
// Unknown symbolic aliases fail closed instead of being treated as independent.
func (r *Resolver) CanonicalModelIdentity(model string) (string, error) {
	expanded, err := r.expandModelAlias(model)
	if err != nil {
		return "", err
	}
	expanded, err = r.expandModelAlias(snapshotSuffix.ReplaceAllString(expanded, ""))
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(expanded, "gpt-") || strings.HasPrefix(expanded, "claude-") || strings.HasPrefix(expanded, "kimi-") || strings.HasPrefix(expanded, "kimi/") {
		return expanded, nil
	}
	return "", fmt.Errorf("unknown model identity %q: configure dispatch.model_aliases with a concrete model ID", model)
}

// ResolveDispatchRoleForProducer is the executable role contract. It pins
// configured aliases and removes all same-model validators, including fallbacks.
// Legacy tier resolution remains unchanged for model-only consumers.
func (r *Resolver) ResolveDispatchRoleForProducer(role, producer string) (ResolvedDispatch, error) {
	resolved, ok := r.ResolveDispatchRole(role)
	if !ok {
		return ResolvedDispatch{}, fmt.Errorf("role %q: no dispatch profile found", role)
	}
	review := role == "validation" || role == "cross-lab-review" || role == "plan-review"
	if review && producer == "" {
		return ResolvedDispatch{}, fmt.Errorf("role %q requires --producer-identity", role)
	}
	if producer != "" {
		identity, err := r.CanonicalModelIdentity(producer)
		if err != nil {
			return ResolvedDispatch{}, fmt.Errorf("producer: %w", err)
		}
		resolved.ProducerIdentity, resolved.ProducerModel = producer, identity
	}
	candidates := append([]DispatchCandidate{{ProfileRef: resolved.ProfileRef, Profile: resolved.Profile}}, resolved.FallbackChain...)
	eligible := []DispatchCandidate{}
	for i, candidate := range candidates {
		model, err := r.expandModelAlias(candidate.Profile.Model)
		if err != nil {
			return ResolvedDispatch{}, err
		}
		candidate.Profile.Model = model
		identity, err := r.CanonicalModelIdentity(model)
		if err != nil && (review || producer != "") {
			return ResolvedDispatch{}, fmt.Errorf("profile %q: %w", candidate.ProfileRef, err)
		}
		candidate.Profile.ModelIdentity = identity
		if candidate.Profile.Role == "" {
			candidate.Profile.Role = role
		}
		if producer != "" && identity == resolved.ProducerModel {
			resolved.Excluded = append(resolved.Excluded, ExcludedDispatchCandidate{candidate, "producer_model_conflict"})
			if i == 0 {
				resolved.FallbackReason = "producer_model_conflict"
			}
			continue
		}
		eligible = append(eligible, candidate)
	}
	if len(eligible) == 0 {
		return ResolvedDispatch{}, fmt.Errorf("role %q has no model distinct from producer %q", role, producer)
	}
	if producer != "" && slices.Contains(r.cfg.Dispatch.CrossLabFirst, role) {
		primaryBeforeReorder := eligible[0].ProfileRef
		eligible, resolved.CrossLabReorder = r.crossLabFirst(eligible, resolved.ProducerModel)
		// A reorder can promote a different eligible candidate to primary. Record
		// that as the fallback reason, but never clobber a reason already set
		// above by the producer-conflict exclusion: that reason explains why the
		// originally-first candidate was dropped at all, which is more important
		// than the policy reordering that picked among what remained. When the
		// reorder does not move the primary, FallbackReason is left untouched.
		if eligible[0].ProfileRef != primaryBeforeReorder && resolved.FallbackReason == "" {
			resolved.FallbackReason = "cross_lab_reorder"
		}
	}
	resolved.ProfileRef, resolved.Profile = eligible[0].ProfileRef, eligible[0].Profile
	resolved.FallbackChain = eligible[1:]
	if review {
		resolved.ValidatorRelationship = "different-model"
	}
	return resolved, nil
}

// CandidateReorder records a policy reordering of eligible profiles by reference.
type CandidateReorder struct {
	From []string `json:"from"`
	To   []string `json:"to"`
}

// modelLab names the lab behind a canonical model identity, or "" if unknown.
func modelLab(identity string) string {
	switch {
	case strings.HasPrefix(identity, "gpt-"):
		return "openai"
	case strings.HasPrefix(identity, "claude-"):
		return "anthropic"
	case strings.HasPrefix(identity, "kimi"):
		return "moonshot"
	}
	return ""
}

// crossLabFirst stably moves candidates from a frontier lab other than the
// producer's ahead of the rest, so a review role reaches another lab before a
// same-lab seat. Non-frontier labs keep their policy position. Policy order is
// otherwise preserved, and a nil reorder means the order did not change.
func (r *Resolver) crossLabFirst(eligible []DispatchCandidate, producerModel string) ([]DispatchCandidate, *CandidateReorder) {
	producerLab := modelLab(producerModel)
	if producerLab == "" {
		return eligible, nil
	}
	frontierLabs := map[string]bool{}
	for _, m := range r.cfg.Reasoning.FrontierModels {
		if id, err := r.CanonicalModelIdentity(m); err == nil && modelLab(id) != "" {
			frontierLabs[modelLab(id)] = true
		}
	}
	var first, rest []DispatchCandidate
	for _, c := range eligible {
		lab := modelLab(c.Profile.ModelIdentity)
		if lab != producerLab && frontierLabs[lab] {
			first = append(first, c)
		} else {
			rest = append(rest, c)
		}
	}
	ordered := append(first, rest...)
	refs := func(cs []DispatchCandidate) []string {
		out := make([]string, len(cs))
		for i, c := range cs {
			out[i] = c.ProfileRef
		}
		return out
	}
	if slices.Equal(refs(eligible), refs(ordered)) {
		return eligible, nil
	}
	return ordered, &CandidateReorder{From: refs(eligible), To: refs(ordered)}
}
