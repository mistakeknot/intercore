package orchcompat

import (
	"os"
	"path/filepath"
	"testing"
)

func baseArgs(extra ...string) []string {
	return append([]string{"--json", "route", "dispatch", "--policy={POLICY}"}, extra...)
}

func TestDispatchRoleErrors(t *testing.T) {
	s := newSandbox(t)
	cases := []runcase{
		{Label: "unsupported role", Args: baseArgs("--role=not-a-role")},
		{Label: "unsupported role with reasons", Args: baseArgs("--role=not-a-role"), Ctx: contextForReason("capability-failure")},
		{Label: "review role without producer", Args: baseArgs("--role=validation")},
		{Label: "review role with unknown producer", Args: baseArgs("--role=validation", "--producer-identity=mystery-model")},
		{Label: "review role with empty producer", Args: baseArgs("--role=cross-lab-review", "--producer-identity=")},
		{Label: "no role tier or type", Args: baseArgs()},
		{Label: "empty role value", Args: baseArgs("--role=")},
		{Label: "unknown reason", Args: baseArgs("--role=planning"), Ctx: `{"reasons":["made-up-reason"],"rationale":"synthetic characterization rationale"}`},
		{Label: "known and unknown reason", Args: baseArgs("--role=planning"), Ctx: `{"reasons":["capability-failure","made-up-reason"],"rationale":"synthetic characterization rationale"}`},
		{Label: "blank rationale", Args: baseArgs("--role=planning"), Ctx: `{"reasons":["capability-failure"],"rationale":""}`},
		{Label: "whitespace rationale", Args: baseArgs("--role=planning"), Ctx: `{"reasons":["capability-failure"],"rationale":"   \t "}`},
		{Label: "missing rationale", Args: baseArgs("--role=planning"), Ctx: `{"reasons":["capability-failure"]}`},
		{Label: "rationale without reasons", Args: baseArgs("--role=planning"), Ctx: `{"rationale":"synthetic characterization rationale"}`},
		{Label: "empty reasons list", Args: baseArgs("--role=planning"), Ctx: `{"reasons":[],"rationale":""}`},
		{Label: "duplicate reasons", Args: baseArgs("--role=planning"), Ctx: `{"reasons":["capability-failure","capability-failure"],"rationale":"synthetic characterization rationale"}`},
		{Label: "all five reasons", Args: baseArgs("--role=planning"), Ctx: `{"reasons":["unresolved-success-criteria","foundational-invariants","broad-consequences","difficult-verification","capability-failure"],"rationale":"synthetic characterization rationale"}`},
		{Label: "malformed context text", Args: baseArgs("--role=planning"), Ctx: `not json`},
		{Label: "malformed context truncated", Args: baseArgs("--role=planning"), Ctx: `{"reasons":["capability-failure"`},
		{Label: "context unknown field", Args: baseArgs("--role=planning"), Ctx: `{"reasons":[],"surprise":true}`},
		{Label: "context two objects", Args: baseArgs("--role=planning"), Ctx: `{} {}`},
		{Label: "context trailing text", Args: baseArgs("--role=planning"), Ctx: `{} trailing`},
		{Label: "context array", Args: baseArgs("--role=planning"), Ctx: `[]`},
		{Label: "context null", Args: baseArgs("--role=planning"), Ctx: `null`},
		{Label: "context wrong type for reasons", Args: baseArgs("--role=planning"), Ctx: `{"reasons":"capability-failure"}`},
		{Label: "context nonexistent file", Args: baseArgs("--role=planning", "--context-file={ROOT}/ctx/none.json")},
		{Label: "context file is a directory", Args: baseArgs("--role=planning", "--context-file={ROOT}/ctx")},
		{Label: "context empty object", Args: baseArgs("--role=planning"), Ctx: `{}`},
		{Label: "investigation without reasons", Args: baseArgs("--role=planning"), Ctx: `{"investigation_active":true}`},
		{Label: "investigation with reasons", Args: baseArgs("--role=planning"), Ctx: `{"investigation_active":true,"reasons":["unresolved-success-criteria"],"rationale":"synthetic characterization rationale"}`},
		{Label: "investigation routine", Args: baseArgs("--role=routine-execution"), Ctx: `{"investigation_active":true,"reasons":["unresolved-success-criteria"],"rationale":"synthetic characterization rationale"}`},
		{Label: "investigation scout", Args: baseArgs("--role=scout"), Ctx: `{"investigation_active":true,"reasons":["unresolved-success-criteria"],"rationale":"synthetic characterization rationale"}`},
	}
	s.runAll("dispatch-errors", cases)
}

func TestDispatchContextShapes(t *testing.T) {
	s := newSandbox(t)
	reasons := `"reasons":["capability-failure"],"rationale":"synthetic characterization rationale"`
	cases := []runcase{
		{Label: "handoff ready routine", Args: baseArgs("--role=routine-execution"), Ctx: `{` + reasons + `,"handoff":{"decisions":"d","constraints":"c","verification":"v","escalation":"e"}}`},
		{Label: "handoff partial routine", Args: baseArgs("--role=routine-execution"), Ctx: `{` + reasons + `,"handoff":{"decisions":"d"}}`},
		{Label: "handoff ready deep", Args: baseArgs("--role=deep-execution"), Ctx: `{` + reasons + `,"handoff":{"decisions":"d","constraints":"c","verification":"v","escalation":"e"}}`},
		{Label: "handoff without reasons", Args: baseArgs("--role=routine-execution"), Ctx: `{"handoff":{"decisions":"d","constraints":"c","verification":"v","escalation":"e"}}`},
		{Label: "domain and scope", Args: baseArgs("--role=planning"), Ctx: `{` + reasons + `,"domain":"synthetic-domain","scope":"synthetic-scope"}`},
		{Label: "available models unprobed", Args: baseArgs("--role=planning"), Ctx: `{` + reasons + `}`},
		{Label: "available models empty", Args: baseArgs("--role=planning"), Ctx: `{` + reasons + `,"available_models":[]}`},
		{Label: "available models claude only", Args: baseArgs("--role=planning"), Ctx: `{` + reasons + `,"available_models":["claude-opus-5-5"]}`},
		{Label: "available models codex only", Args: baseArgs("--role=frontier-planning"), Ctx: `{` + reasons + `,"available_models":["gpt-6-astra"]}`},
		{Label: "available models none match", Args: baseArgs("--role=frontier-planning"), Ctx: `{` + reasons + `,"available_models":["gpt-9-unknown"]}`},
		{Label: "available models sonnet execution", Args: baseArgs("--role=routine-execution"), Ctx: `{"available_models":["claude-sonnet-5-5"]}`},
		{Label: "available models sol only deep", Args: baseArgs("--role=deep-execution"), Ctx: `{"available_models":["gpt-6.1-sol"]}`},
		{Label: "available models empty routine", Args: baseArgs("--role=routine-execution"), Ctx: `{"available_models":[]}`},
		{Label: "available models alias", Args: baseArgs("--role=routine-execution"), Ctx: `{"available_models":["sonnet"]}`},
		{Label: "reason with sonnet executor", Args: baseArgs("--role=deep-execution"), Ctx: `{"reasons":["broad-consequences"],"rationale":"synthetic characterization rationale","available_models":["claude-sonnet-5-5","claude-opus-5-5"]}`},
		{Label: "explicit effort override in context", Args: baseArgs("--role=planning"), Ctx: `{` + reasons + `,"effort_override":"medium"}`},
		{Label: "context from stdin path dash", Args: baseArgs("--role=planning", "--context-file=-")},
	}
	s.runAll("dispatch-context-shapes", cases)
}

func TestDispatchEffortOverride(t *testing.T) {
	s := newSandbox(t)
	floor := `{"reasons":["foundational-invariants"],"rationale":"synthetic characterization rationale"}`
	cases := []runcase{
		{Label: "override low no floor", Args: baseArgs("--role=planning", "--effort-override=low")},
		{Label: "override medium no floor", Args: baseArgs("--role=planning", "--effort-override=medium")},
		{Label: "override high no floor", Args: baseArgs("--role=planning", "--effort-override=high")},
		{Label: "override xhigh no floor", Args: baseArgs("--role=planning", "--effort-override=xhigh")},
		{Label: "override invalid level", Args: baseArgs("--role=planning", "--effort-override=extreme")},
		{Label: "override uppercase", Args: baseArgs("--role=planning", "--effort-override=HIGH")},
		{Label: "override empty value", Args: baseArgs("--role=planning", "--effort-override=")},
		{Label: "override without role", Args: baseArgs("--tier=fast", "--effort-override=high")},
		{Label: "override low under floor", Args: baseArgs("--role=planning", "--effort-override=low"), Ctx: floor},
		{Label: "override xhigh under floor", Args: baseArgs("--role=planning", "--effort-override=xhigh"), Ctx: floor},
		{Label: "override on claude seat", Args: baseArgs("--role=routine-execution", "--effort-override=xhigh")},
		{Label: "override on claude seat medium", Args: baseArgs("--role=coordination", "--effort-override=medium")},
		{Label: "override on claude seat low", Args: baseArgs("--role=coordination", "--effort-override=low")},
		{Label: "override with producer", Args: baseArgs("--role=validation", "--producer-identity=gpt-6.1-sol", "--effort-override=high")},
	}
	s.runAll("dispatch-effort-override", cases)
}

func TestDispatchEffortFloors(t *testing.T) {
	s := newSandbox(t)
	var cases []runcase
	for _, reason := range []string{"unresolved-success-criteria", "foundational-invariants", "broad-consequences", "difficult-verification", "capability-failure"} {
		for _, role := range []string{"cross-lab-review", "coordination", "routine-execution", "scout", "release-authority"} {
			args := baseArgs("--role=" + role)
			if role == "cross-lab-review" {
				args = append(args, "--producer-identity=claude-opus-5-5")
			}
			cases = append(cases, runcase{Label: role + " reason=" + reason, Args: args, Ctx: contextForReason(reason)})
		}
	}
	cases = append(cases, runcase{
		Label: "cross-lab-review exemption keeps other floors",
		Args:  baseArgs("--role=cross-lab-review", "--producer-identity=claude-opus-5-5"),
		Ctx:   `{"reasons":["difficult-verification","foundational-invariants"],"rationale":"synthetic characterization rationale"}`,
	})
	cases = append(cases, runcase{
		Label: "cross-lab-review exemption alone",
		Args:  baseArgs("--role=cross-lab-review", "--producer-identity=claude-opus-5-5"),
		Ctx:   `{"reasons":["difficult-verification"],"rationale":"synthetic characterization rationale"}`,
	})
	s.runAll("dispatch-effort-floors", cases)
}

func TestDispatchPolicyProfiles(t *testing.T) {
	s := newSandbox(t)
	rationale := `"rationale":"synthetic characterization rationale"`
	cases := []runcase{
		{Label: "explicit default profile", Args: baseArgs("--role=main-integrator", "--policy-profile=default")},
		{Label: "unknown profile", Args: baseArgs("--role=main-integrator", "--policy-profile=missing-profile")},
		{Label: "pilot profile without scope", Args: baseArgs("--role=main-integrator", "--policy-profile=ci-campaign-pilot")},
		{Label: "pilot profile wrong scope", Args: baseArgs("--role=main-integrator", "--policy-profile=ci-campaign-pilot"), Ctx: `{"scope":"other-scope"}`},
		{Label: "pilot profile main-integrator", Args: baseArgs("--role=main-integrator", "--policy-profile=ci-campaign-pilot"), Ctx: `{"scope":"pilot-campaign"}`},
		{Label: "pilot profile routine-execution", Args: baseArgs("--role=routine-execution", "--policy-profile=ci-campaign-pilot"), Ctx: `{"scope":"pilot-campaign"}`},
		{Label: "pilot profile validation", Args: baseArgs("--role=validation", "--policy-profile=ci-campaign-pilot", "--producer-identity=gpt-6.1-sol"), Ctx: `{"scope":"pilot-campaign"}`},
		{Label: "pilot profile validation claude producer", Args: baseArgs("--role=validation", "--policy-profile=ci-campaign-pilot", "--producer-identity=claude-opus-5-5"), Ctx: `{"scope":"pilot-campaign"}`},
		{Label: "pilot profile unmapped role", Args: baseArgs("--role=planning", "--policy-profile=ci-campaign-pilot"), Ctx: `{"scope":"pilot-campaign"}`},
		{Label: "pilot profile main-integrator foundational", Args: baseArgs("--role=main-integrator", "--policy-profile=ci-campaign-pilot"), Ctx: `{"scope":"pilot-campaign","reasons":["foundational-invariants"],` + rationale + `}`},
		{Label: "pilot profile routine elevated", Args: baseArgs("--role=routine-execution", "--policy-profile=ci-campaign-pilot"), Ctx: `{"scope":"pilot-campaign","reasons":["capability-failure"],` + rationale + `}`},
		{Label: "pilot profile routine unavailable codex", Args: baseArgs("--role=validation", "--policy-profile=ci-campaign-pilot", "--producer-identity=claude-opus-5-5"), Ctx: `{"scope":"pilot-campaign","available_models":["claude-opus-5-5"]}`},
	}
	s.runAll("dispatch-policy-profiles", cases)
}

func TestDispatchProducerIdentities(t *testing.T) {
	s := newSandbox(t)
	var cases []runcase
	for _, producer := range []string{
		"gpt-6.1-sol", "gpt-6-astra", "claude-opus-5-5", "claude-sonnet-5-5", "opus", "sonnet", "k3", "kimi/k3",
		"kimi-code/k3", "codex/gpt-6.1-sol", "openai:gpt-6.1-sol", "anthropic/claude-opus-5-5", "claude-code/claude-opus-5-5",
		"claude-opus-5-5[1m]", "claude-opus-5-5[200k]", "CLAUDE-OPUS-5-5", "claude-opus-5-5-2026-09-25", "claude-opus-5-5-20260925",
		"gpt-6.1-sol-2026-09-29", "gemini-3", "unknown",
	} {
		for _, role := range []string{"validation", "plan-review", "cross-lab-review"} {
			cases = append(cases, runcase{
				Label: role + " producer=" + producer,
				Args:  baseArgs("--role="+role, "--producer-identity="+producer),
			})
		}
	}
	s.runAll("dispatch-producer-identities", cases)
}

func TestDispatchLegacyTierAndType(t *testing.T) {
	s := newSandbox(t)
	shape := loadFixtureShape(t)
	var cases []runcase
	for _, tier := range sortedKeys(shape.Dispatch.Tiers) {
		cases = append(cases, runcase{Label: "tier=" + tier, Args: []string{"--json", "route", "dispatch", "--policy={POLICY}", "--tier=" + tier}})
	}
	cases = append(cases,
		runcase{Label: "tier text", Args: []string{"route", "dispatch", "--policy={POLICY}", "--tier=fast"}},
		runcase{Label: "tier positional", Args: []string{"route", "dispatch", "--policy={POLICY}", "deep"}},
		runcase{Label: "tier fallback alias", Args: []string{"--json", "route", "dispatch", "--policy={POLICY}", "--tier=fast-alt"}},
		runcase{Label: "tier missing", Args: []string{"--json", "route", "dispatch", "--policy={POLICY}", "--tier=no-such-tier"}},
		runcase{Label: "tier missing text", Args: []string{"route", "dispatch", "--policy={POLICY}", "--tier=no-such-tier"}},
		runcase{Label: "type unknown json", Args: []string{"--json", "route", "dispatch", "--policy={POLICY}", "--type=no-such-type"}},
		runcase{Label: "type unknown text", Args: []string{"route", "dispatch", "--policy={POLICY}", "--type=no-such-type"}},
		runcase{Label: "type phase unknown", Args: []string{"--json", "route", "dispatch", "--policy={POLICY}", "--type=no-such-type", "--phase=planned"}},
		runcase{Label: "type tier name", Args: []string{"--json", "route", "dispatch", "--policy={POLICY}", "--type=fast"}},
		runcase{Label: "type tier name text", Args: []string{"route", "dispatch", "--policy={POLICY}", "--type=deep"}},
		runcase{Label: "no selector", Args: []string{"--json", "route", "dispatch", "--policy={POLICY}"}},
	)
	s.runAll("dispatch-legacy-tier-type", cases)
}

func TestDispatchCalibrationFlag(t *testing.T) {
	s := newSandbox(t)
	missing := filepath.Join(s.root, "none", "calibration.json")
	cases := []runcase{
		{Label: "calibration missing file", Args: baseArgs("--role=planning", "--calibration="+missing)},
		{Label: "calibration missing file validation", Args: baseArgs("--role=validation", "--producer-identity=gpt-6.1-sol", "--calibration="+missing)},
	}
	s.runAll("dispatch-calibration-flag", cases)
}

func TestDispatchPolicyFiles(t *testing.T) {
	s := newSandbox(t)
	write := func(name, content string) string {
		path := filepath.Join(s.root, "policies", name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	empty := write("empty.yaml", "")
	notYAML := write("not-yaml.yaml", "reasoning: [unterminated\n")
	noRoles := write("no-roles.yaml", "reasoning:\n  frontier_models: [gpt-6-astra]\n  frontier_reasons: [capability-failure]\n")
	badFrontier := write("bad-frontier.yaml", "reasoning:\n  frontier_models: [mystery-model]\n  frontier_reasons: [capability-failure]\ndispatch:\n  roles: {planning: p}\n  tiers:\n    p: {backend: codex, model: gpt-6-astra, reasoning_effort: high}\n")
	badFloor := write("bad-floor.yaml", "reasoning:\n  frontier_models: [gpt-6-astra]\n  frontier_reasons: [capability-failure]\ndispatch:\n  effort_floors: {not-a-reason: high}\n  roles: {planning: p}\n  tiers:\n    p: {backend: codex, model: gpt-6-astra, reasoning_effort: high}\n")
	badRoleTier := write("bad-role-tier.yaml", "reasoning:\n  frontier_models: [gpt-6-astra]\n  frontier_reasons: [capability-failure]\ndispatch:\n  roles: {planning: missing-tier}\n  tiers:\n    p: {backend: codex, model: gpt-6-astra, reasoning_effort: high}\n")
	dir := filepath.Join(s.root, "policies")
	cases := []runcase{
		{Label: "nonexistent policy", Args: []string{"--json", "route", "dispatch", "--policy={ROOT}/policies/absent.yaml", "--role=planning"}},
		{Label: "policy is a directory", Args: []string{"--json", "route", "dispatch", "--policy=" + dir, "--role=planning"}},
		{Label: "empty policy file", Args: []string{"--json", "route", "dispatch", "--policy=" + empty, "--role=planning"}},
		{Label: "policy not yaml", Args: []string{"--json", "route", "dispatch", "--policy=" + notYAML, "--role=planning"}},
		{Label: "policy without roles", Args: []string{"--json", "route", "dispatch", "--policy=" + noRoles, "--role=planning"}},
		{Label: "policy with unknown frontier model", Args: []string{"--json", "route", "dispatch", "--policy=" + badFrontier, "--role=planning"}},
		{Label: "policy with unknown floor reason", Args: []string{"--json", "route", "dispatch", "--policy=" + badFloor, "--role=planning"}},
		{Label: "policy role names missing tier", Args: []string{"--json", "route", "dispatch", "--policy=" + badRoleTier, "--role=planning"}},
		{Label: "no policy selected", Args: []string{"--json", "route", "dispatch", "--role=planning"}},
		{Label: "model on missing policy", Args: []string{"--json", "route", "model", "--policy={ROOT}/policies/absent.yaml", "--phase=planned", "--category=review", "--agent=fd-safety"}},
		{Label: "batch on missing policy", Args: []string{"--json", "route", "batch", "--policy={ROOT}/policies/absent.yaml", "--phase=planned", "fd-safety"}},
		{Label: "table on missing policy", Args: []string{"--json", "route", "table", "--policy={ROOT}/policies/absent.yaml"}},
		{Label: "identity on missing policy", Args: []string{"--json", "route", "identity", "--policy={ROOT}/policies/absent.yaml", "--model=opus"}},
		{Label: "tier on missing policy", Args: []string{"--json", "route", "dispatch", "--policy={ROOT}/policies/absent.yaml", "--tier=fast"}},
		{Label: "type on missing policy", Args: []string{"--json", "route", "dispatch", "--policy={ROOT}/policies/absent.yaml", "--type=fast"}},
	}
	s.runAll("dispatch-policy-files", cases)
}
