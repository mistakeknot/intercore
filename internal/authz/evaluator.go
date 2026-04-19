package authz

import (
	"fmt"
	"time"
)

const defaultClockSkewTolerance = 5 * time.Minute

// CheckInput is the runtime context passed to policy checks.
type CheckInput struct {
	Op                string
	Target            string
	BeadID            string
	Now               time.Time
	HeadSHA           string
	VettedSHA         string
	VettedAt          time.Time
	TestsPassed       bool
	SprintOrWorkFlow  bool
	ClockSkewTolerance time.Duration
	VettingSHAs       map[string]string
	WorkdirHEAD       map[string]string
}

// Check returns the policy decision for an operation.
func Check(policy *Policy, input CheckInput) (CheckResult, error) {
	if policy == nil {
		return CheckResult{}, fmt.Errorf("nil policy")
	}

	if input.Now.IsZero() {
		input.Now = time.Now()
	}

	rule, idx := matchRule(policy.Rules, input.Op)
	if rule == nil {
		return CheckResult{Mode: ModeConfirm, Reason: "no matching rule"}, nil
	}

	result := CheckResult{
		Mode:        rule.Mode,
		PolicyMatch: fmt.Sprintf("%s#%d", rule.Op, idx),
	}

	if rule.Mode == ModeBlock {
		result.Reason = "rule mode is block"
		return result, nil
	}

	ok, reason := Evaluate(*rule, input)
	if !ok {
		result.Mode = ModeConfirm
		result.Reason = reason
		return result, nil
	}

	result.Reason = "requires satisfied"
	return result, nil
}

// Evaluate checks whether a single rule's requirements are satisfied.
func Evaluate(rule Rule, input CheckInput) (bool, string) {
	if len(rule.Requires) == 0 {
		return true, "no requirements"
	}
	if input.Now.IsZero() {
		input.Now = time.Now()
	}
	tol := input.ClockSkewTolerance
	if tol <= 0 {
		tol = defaultClockSkewTolerance
	}

	for key, raw := range rule.Requires {
		switch key {
		case "vetted_within_minutes":
			mins, ok := asInt(raw)
			if !ok || mins < 0 {
				return false, "invalid vetted_within_minutes"
			}
			if input.VettedAt.IsZero() {
				return false, "missing vetted_at"
			}
			maxAge := time.Duration(mins)*time.Minute + tol
			age := input.Now.Sub(input.VettedAt)
			if age > maxAge {
				return false, fmt.Sprintf("vetting too old: %s > %s", age.Round(time.Second), maxAge.Round(time.Second))
			}
		case "vetted_sha_matches_head":
			req, ok := asBool(raw)
			if !ok {
				return false, "invalid vetted_sha_matches_head"
			}
			if req {
				if len(input.VettingSHAs) > 0 {
					if len(input.WorkdirHEAD) == 0 {
						return false, "missing workdir heads"
					}
					for repo, vetted := range input.VettingSHAs {
						head, ok := input.WorkdirHEAD[repo]
						if !ok || head != vetted {
							return false, fmt.Sprintf("vetted sha mismatch for %s", repo)
						}
					}
				} else if input.VettedSHA == "" || input.HeadSHA == "" || input.VettedSHA != input.HeadSHA {
					return false, "vetted sha mismatch"
				}
			}
		case "tests_passed":
			req, ok := asBool(raw)
			if !ok {
				return false, "invalid tests_passed"
			}
			if input.TestsPassed != req {
				return false, "tests_passed requirement failed"
			}
		case "sprint_or_work_flow":
			req, ok := asBool(raw)
			if !ok {
				return false, "invalid sprint_or_work_flow"
			}
			if input.SprintOrWorkFlow != req {
				return false, "sprint_or_work_flow requirement failed"
			}
		case "committed_by_this_session":
			// Placeholder in v1 package; command-layer check can enrich this signal.
			req, ok := asBool(raw)
			if !ok {
				return false, "invalid committed_by_this_session"
			}
			if req {
				return false, "committed_by_this_session unsupported in evaluator"
			}
		default:
			return false, fmt.Sprintf("unsupported requirement: %s", key)
		}
	}

	return true, "requires satisfied"
}

func matchRule(rules []Rule, op string) (*Rule, int) {
	for i := range rules {
		if rules[i].Op == op {
			return &rules[i], i
		}
	}
	for i := range rules {
		if rules[i].Op == "*" {
			return &rules[i], i
		}
	}
	return nil, -1
}
