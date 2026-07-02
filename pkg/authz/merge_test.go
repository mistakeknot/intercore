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

// TestMerge_BooleanAND covers the spec in docs/canon/policy-merge.md Example 2:
// a required boolean (base=true) that the child drops is only permitted when
// the base rule sets allow_override:true. Per spec, a successfully dropped
// requirement is removed from the merged requires map entirely
// (drop-to-absent); "requires X=false" has no useful semantics.
func TestMerge_BooleanAND(t *testing.T) {
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
	if _, exists := merged.Rules[idx].Requires["tests_passed"]; exists {
		t.Fatalf("tests_passed should be absent after allowed override drop; got %v", merged.Rules[idx].Requires["tests_passed"])
	}

	// Explicit false from child with allow_override is normalized to absent.
	projectFalse := &Policy{
		Version: 1,
		Rules: []Rule{
			{Op: "bead-close", Mode: ModeAuto, Requires: map[string]interface{}{"tests_passed": false}},
		},
	}
	merged2, err := MergePolicies(globalAllow, projectFalse)
	if err != nil {
		t.Fatalf("MergePolicies with explicit false: %v", err)
	}
	idx2 := findRuleByOp(merged2.Rules, "bead-close")
	if _, exists := merged2.Rules[idx2].Requires["tests_passed"]; exists {
		t.Fatalf("tests_passed should be absent after explicit-false override; got %v", merged2.Rules[idx2].Requires["tests_passed"])
	}

	// Without allow_override, explicit false is rejected.
	if _, err := MergePolicies(global, projectFalse); err == nil {
		t.Fatal("expected merge to reject tests_passed=false without allow_override")
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
