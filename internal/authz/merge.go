package authz

import (
	"fmt"
)

// MergePolicies merges policy layers in ascending precedence order.
// Later policies override earlier ones within floor constraints.
func MergePolicies(policies ...*Policy) (*Policy, error) {
	merged := &Policy{Version: 1, Rules: []Rule{}}
	for _, p := range policies {
		if p == nil {
			continue
		}
		if p.Version > 0 {
			merged.Version = p.Version
		}
		for _, incoming := range p.Rules {
			if incoming.Op == "" {
				return nil, fmt.Errorf("rule op cannot be empty")
			}
			if !isValidMode(incoming.Mode) {
				return nil, fmt.Errorf("invalid mode %q for op %q", incoming.Mode, incoming.Op)
			}
			if incoming.Requires == nil {
				incoming.Requires = map[string]interface{}{}
			}

			idx := findRuleByOp(merged.Rules, incoming.Op)
			if idx < 0 {
				merged.Rules = insertRule(merged.Rules, incoming)
				continue
			}

			next, err := mergeRule(merged.Rules[idx], incoming)
			if err != nil {
				return nil, err
			}
			merged.Rules[idx] = next
		}
	}
	return merged, nil
}

func mergeRule(base, override Rule) (Rule, error) {
	merged := deepCopyRule(base)

	if base.Op != override.Op {
		return Rule{}, fmt.Errorf("cannot merge rules with different op selectors: %q vs %q", base.Op, override.Op)
	}

	if err := validateModeTransition(base, override); err != nil {
		return Rule{}, err
	}
	merged.Mode = override.Mode
	merged.AllowOverride = base.AllowOverride || override.AllowOverride

	requires, err := mergeRequires(base.Requires, override.Requires, base.AllowOverride)
	if err != nil {
		return Rule{}, fmt.Errorf("merge requires for op %q: %w", base.Op, err)
	}
	merged.Requires = requires
	return merged, nil
}

func validateModeTransition(base, override Rule) error {
	if base.AllowOverride {
		return nil
	}

	if base.Mode == ModeConfirm || base.Mode == ModeBlock {
		if override.Mode == ModeAuto || override.Mode == ModeForceAuto {
			return fmt.Errorf("op %q cannot relax mode %q to %q without allow_override", base.Op, base.Mode, override.Mode)
		}
	}

	return nil
}

func mergeRequires(base, override map[string]interface{}, allowOverride bool) (map[string]interface{}, error) {
	out := map[string]interface{}{}
	for k, v := range base {
		out[k] = v
	}

	for key, oldVal := range base {
		oldBool, ok := asBool(oldVal)
		if !ok || !oldBool {
			continue
		}
		_, exists := override[key]
		if !exists && !allowOverride {
			return nil, fmt.Errorf("cannot remove required boolean %q without allow_override", key)
		}
	}

	for key, newVal := range override {
		oldVal, exists := base[key]
		if !exists {
			out[key] = newVal
			continue
		}

		if oldNum, ok := asInt(oldVal); ok {
			if newNum, ok := asInt(newVal); ok {
				if newNum < oldNum {
					out[key] = newNum
				} else {
					out[key] = oldNum
				}
				continue
			}
		}

		if oldBool, ok := asBool(oldVal); ok {
			if newBool, ok := asBool(newVal); ok {
				if oldBool && !newBool && !allowOverride {
					return nil, fmt.Errorf("cannot relax boolean %q from true to false without allow_override", key)
				}
				out[key] = oldBool && newBool
				continue
			}
		}

		out[key] = newVal
	}

	return out, nil
}

func insertRule(rules []Rule, incoming Rule) []Rule {
	if incoming.Op == "*" {
		return append(rules, incoming)
	}
	catchallIdx := findRuleByOp(rules, "*")
	if catchallIdx < 0 {
		return append(rules, incoming)
	}
	out := make([]Rule, 0, len(rules)+1)
	out = append(out, rules[:catchallIdx]...)
	out = append(out, incoming)
	out = append(out, rules[catchallIdx:]...)
	return out
}

func findRuleByOp(rules []Rule, op string) int {
	for i, rule := range rules {
		if rule.Op == op {
			return i
		}
	}
	return -1
}

func isValidMode(mode string) bool {
	switch mode {
	case ModeAuto, ModeConfirm, ModeBlock, ModeForceAuto:
		return true
	default:
		return false
	}
}

func deepCopyRule(rule Rule) Rule {
	copyRule := rule
	copyRule.Requires = map[string]interface{}{}
	for k, v := range rule.Requires {
		copyRule.Requires[k] = v
	}
	return copyRule
}

func asInt(v interface{}) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	case float32:
		return int(n), true
	default:
		return 0, false
	}
}

func asBool(v interface{}) (bool, bool) {
	b, ok := v.(bool)
	return b, ok
}
