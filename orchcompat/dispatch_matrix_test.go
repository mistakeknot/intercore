package orchcompat

import (
	"fmt"
	"sort"
	"testing"
)

var reviewRoles = map[string]bool{"plan-review": true, "validation": true, "cross-lab-review": true}

var producerIdentities = []string{"gpt-6.1-sol", "claude-opus-5-5"}

const matrixRationale = "synthetic characterization rationale"

func contextForReason(reason string) string {
	if reason == "" {
		return ""
	}
	return fmt.Sprintf(`{"reasons":[%q],"rationale":%q}`, reason, matrixRationale)
}

func TestFixtureShapeIsPinned(t *testing.T) {
	shape := loadFixtureShape(t)
	wantRoles := []string{
		"coordination", "coordinator-seat", "cross-lab-review", "deep-execution", "escalation",
		"frontier-planning", "lane", "main-integrator", "main-session", "plan-review", "planning",
		"release-authority", "release-preparation", "routine-execution", "scout", "validation",
	}
	if got := sortedKeys(shape.Dispatch.Roles); fmt.Sprint(got) != fmt.Sprint(wantRoles) {
		t.Fatalf("fixture roles changed: %v", got)
	}
	wantReasons := []string{"unresolved-success-criteria", "foundational-invariants", "broad-consequences", "difficult-verification", "capability-failure"}
	if fmt.Sprint(shape.Reasoning.FrontierReasons) != fmt.Sprint(wantReasons) {
		t.Fatalf("fixture reasons changed: %v", shape.Reasoning.FrontierReasons)
	}
	if len(shape.Dispatch.Tiers) != 41 {
		t.Fatalf("fixture tier count = %d", len(shape.Dispatch.Tiers))
	}
}

func TestDispatchRoleMatrix(t *testing.T) {
	shape := loadFixtureShape(t)
	reasons := append([]string{""}, shape.Reasoning.FrontierReasons...)
	roles := sortedKeys(shape.Dispatch.Roles)
	for _, role := range roles {
		t.Run(role, func(t *testing.T) {
			s := newSandbox(t)
			var cases []runcase
			producers := []string{""}
			if reviewRoles[role] {
				producers = append(producers, producerIdentities...)
			}
			for _, producer := range producers {
				for _, reason := range reasons {
					label := "reason=" + reason
					if reason == "" {
						label = "reason=none"
					}
					args := []string{"--json", "route", "dispatch", "--policy={POLICY}", "--role=" + role}
					if producer != "" {
						label += " producer=" + producer
						args = append(args, "--producer-identity="+producer)
					}
					cases = append(cases, runcase{Label: label, Args: args, Ctx: contextForReason(reason)})
				}
			}
			s.runAll("dispatch-role-"+role, cases)
		})
	}
}

func TestDispatchRoleMatrixTextOutput(t *testing.T) {
	shape := loadFixtureShape(t)
	s := newSandbox(t)
	var cases []runcase
	roles := sortedKeys(shape.Dispatch.Roles)
	sort.Strings(roles)
	for _, role := range roles {
		args := []string{"route", "dispatch", "--policy={POLICY}", "--role=" + role}
		if reviewRoles[role] {
			args = append(args, "--producer-identity=gpt-6.1-sol")
		}
		cases = append(cases, runcase{Label: "role=" + role + " text", Args: args})
	}
	s.runAll("dispatch-role-text", cases)
}
