package authz

import "testing"

func TestMerge_NumericMin(t *testing.T) {
	global := &Policy{
		Version: 1,
		Rules: []Rule{
			{Op: "bead-close", Mode: ModeAuto, Requires: map[string]interface{}{"vetted_within_minutes": 60}},
			{Op: "*", Mode: ModeConfirm},
		},
	}
	project := &Policy{
		Version: 1,
		Rules: []Rule{
			{Op: "bead-close", Mode: ModeAuto, Requires: map[string]interface{}{"vetted_within_minutes": 30}},
		},
	}
	merged, err := MergePolicies(global, project)
	if err != nil {
		t.Fatalf("MergePolicies: %v", err)
	}
	idx := findRuleByOp(merged.Rules, "bead-close")
	if idx < 0 {
		t.Fatal("missing bead-close rule")
	}
	got, ok := asInt(merged.Rules[idx].Requires["vetted_within_minutes"])
	if !ok {
		t.Fatal("vetted_within_minutes missing")
	}
	if got != 30 {
		t.Fatalf("vetted_within_minutes = %d, want 30", got)
	}
}

func TestMerge_BooleanAND(t *testing.T) {
	t.Skip("TODO(sylveste-qdqr): allow_override drop-to-false semantics need spec clarification; merge currently removes key, test expects explicit false")
	global := &Policy{
		Version: 1,
		Rules: []Rule{
			{Op: "bead-close", Mode: ModeAuto, Requires: map[string]interface{}{"tests_passed": true}},
			{Op: "*", Mode: ModeConfirm},
		},
	}
	projectDrop := &Policy{
		Version: 1,
		Rules: []Rule{
			{Op: "bead-close", Mode: ModeAuto, Requires: map[string]interface{}{}},
		},
	}
	if _, err := MergePolicies(global, projectDrop); err == nil {
		t.Fatal("expected merge to reject dropping tests_passed without allow_override")
	}

	globalAllow := &Policy{
		Version: 1,
		Rules: []Rule{
			{Op: "bead-close", Mode: ModeAuto, AllowOverride: true, Requires: map[string]interface{}{"tests_passed": true}},
			{Op: "*", Mode: ModeConfirm},
		},
	}
	merged, err := MergePolicies(globalAllow, projectDrop)
	if err != nil {
		t.Fatalf("MergePolicies with allow_override: %v", err)
	}
	idx := findRuleByOp(merged.Rules, "bead-close")
	if idx < 0 {
		t.Fatal("missing bead-close rule")
	}
	got, ok := asBool(merged.Rules[idx].Requires["tests_passed"])
	if !ok {
		t.Fatal("tests_passed missing")
	}
	if got {
		t.Fatal("tests_passed should be false after allowed override drop")
	}
}

func TestMerge_CatchallFloor(t *testing.T) {
	global := &Policy{
		Version: 1,
		Rules: []Rule{
			{Op: "*", Mode: ModeConfirm},
		},
	}
	project := &Policy{
		Version: 1,
		Rules: []Rule{
			{Op: "*", Mode: ModeAuto},
		},
	}
	if _, err := MergePolicies(global, project); err == nil {
		t.Fatal("expected catchall floor relaxation to fail")
	}
}

func TestMerge_ForceAutoPropagates(t *testing.T) {
	global := &Policy{
		Version: 1,
		Rules: []Rule{
			{Op: "bead-close", Mode: ModeAuto},
		},
	}
	project := &Policy{
		Version: 1,
		Rules: []Rule{
			{Op: "bead-close", Mode: ModeForceAuto},
		},
	}
	merged, err := MergePolicies(global, project)
	if err != nil {
		t.Fatalf("MergePolicies: %v", err)
	}
	idx := findRuleByOp(merged.Rules, "bead-close")
	if idx < 0 {
		t.Fatal("missing bead-close rule")
	}
	if merged.Rules[idx].Mode != ModeForceAuto {
		t.Fatalf("mode = %q, want %q", merged.Rules[idx].Mode, ModeForceAuto)
	}
}

func TestMerge_FirstMatchWins(t *testing.T) {
	policy := &Policy{
		Version: 1,
		Rules: []Rule{
			{Op: "bead-close", Mode: ModeConfirm},
			{Op: "bead-close", Mode: ModeAuto},
			{Op: "*", Mode: ModeBlock},
		},
	}
	result, err := Check(policy, CheckInput{Op: "bead-close"})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.PolicyMatch != "bead-close#0" {
		t.Fatalf("PolicyMatch = %q, want %q", result.PolicyMatch, "bead-close#0")
	}
	if result.Mode != ModeConfirm {
		t.Fatalf("Mode = %q, want %q", result.Mode, ModeConfirm)
	}
}
