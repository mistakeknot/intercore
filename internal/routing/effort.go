package routing

import (
	"fmt"
	"slices"
	"strings"
)

// Thresholds share a vocabulary, but profile efforts are compared only within
// the capability order of their backend and canonical model identity.
var effortLevels = []string{"low", "medium", "high", "xhigh"}

// EffortFloorApplication records a transformation, including excluded profiles;
// it does not imply that the candidate was selected or executed.
type EffortFloorApplication struct {
	ProfileRef string   `json:"profile_ref"`
	Reasons    []string `json:"reasons"`
	From       string   `json:"from"`
	To         string   `json:"to"`
}

// EffortOverrideApplication identifies the resolved primary before ordinary
// eligibility filtering. FinalEffort is nil when effort resolution failed.
type EffortOverrideApplication struct {
	ProfileRef  string  `json:"profile_ref"`
	Requested   string  `json:"requested"`
	Applied     bool    `json:"applied"`
	Changed     bool    `json:"changed"`
	From        string  `json:"from"`
	To          string  `json:"to"`
	FinalEffort *string `json:"final_effort"`
}

var backendEffortOrders = map[string][]string{
	"codex/gpt-6-*":          {"low", "medium", "high", "xhigh"},
	"claude/claude-opus-*":   {"medium", "high"},
	"claude/claude-sonnet-*": {"medium", "high"},
	"kimi/kimi-code/k3":      {"high"},
}

func effortOrder(profile DispatchProfile) ([]string, bool) {
	key := ""
	switch {
	case profile.Backend == "codex" && strings.HasPrefix(profile.ModelIdentity, "gpt-6-"):
		key = "codex/gpt-6-*"
	case profile.Backend == "claude" && strings.HasPrefix(profile.ModelIdentity, "claude-opus-"):
		key = "claude/claude-opus-*"
	case profile.Backend == "claude" && strings.HasPrefix(profile.ModelIdentity, "claude-sonnet-"):
		key = "claude/claude-sonnet-*"
	case profile.Backend == "kimi" && profile.ModelIdentity == "kimi-code/k3":
		key = "kimi/kimi-code/k3"
	}
	order, ok := backendEffortOrders[key]
	return order, ok
}

// applyEffortFloor is atomic: a single unrepresentable floor returns the original
// profile and no successful raise reasons, even if other floors were satisfiable.
func applyEffortFloor(profile DispatchProfile, reasons []string, floors map[string]string) (DispatchProfile, []string, error) {
	order, known := effortOrder(profile)
	current := slices.Index(order, profile.ReasoningEffort)
	target := current
	var raisedBy []string
	for _, reason := range reasons {
		floor, matched := floors[reason]
		if !matched {
			continue
		}
		if !known || current < 0 {
			return profile, nil, fmt.Errorf("unsupported_adapter: %s/%s effort %q", profile.Backend, profile.ModelIdentity, profile.ReasoningEffort)
		}
		threshold := slices.Index(effortLevels, floor)
		minimum := -1
		if threshold >= 0 {
			for i, effort := range order {
				if slices.Index(effortLevels, effort) >= threshold {
					minimum = i
					break
				}
			}
		}
		if minimum < 0 {
			return profile, nil, fmt.Errorf("unsupported_adapter: %s/%s cannot enforce floor %q", profile.Backend, profile.ModelIdentity, floor)
		}
		if minimum > current {
			raisedBy = append(raisedBy, reason)
			target = max(target, minimum)
		}
	}
	if len(raisedBy) == 0 {
		return profile, nil, nil
	}
	slices.Sort(raisedBy)
	raisedBy = slices.Compact(raisedBy)
	profile.ReasoningEffort = order[target]
	return profile, raisedBy, nil
}
