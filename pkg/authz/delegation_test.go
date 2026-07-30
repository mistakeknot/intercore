package authz

import (
	"strings"
	"testing"

	"github.com/mistakeknot/intercore/pkg/autonomy"
)

// pushPolicy mirrors the shipped rule for git-push-main: auto, gated only on
// the commits having been made by this session.
func pushPolicy(mode string) *Policy {
	return &Policy{
		Version: 1,
		Rules: []Rule{
			{Op: "git-push-main", Mode: mode, Requires: map[string]interface{}{
				"committed_by_this_session": true,
			}},
			{Op: "*", Mode: ModeConfirm},
		},
	}
}

func level(l int, declared bool) *autonomy.Resolution {
	return &autonomy.Resolution{
		Level:       l,
		Name:        autonomy.Name(l),
		AutoAdvance: autonomy.DerivesAutoAdvance(l),
		Declared:    declared,
	}
}

func pushInput(l *autonomy.Resolution) CheckInput {
	return CheckInput{
		Op:                     "git-push-main",
		Target:                 "repo=sha256:abc;ref=refs/heads/main;head=deadbeef",
		CommittedByThisSession: true,
		Delegation:             l,
	}
}

func TestCheck_DelegationCeilingDowngradesAuto(t *testing.T) {
	for _, l := range []int{0, 1, 2} {
		res, err := Check(pushPolicy(ModeAuto), pushInput(level(l, true)))
		if err != nil {
			t.Fatalf("L%d: Check: %v", l, err)
		}
		if res.Mode != ModeConfirm {
			t.Errorf("L%d: mode = %q, want %q", l, res.Mode, ModeConfirm)
		}
		if !res.DelegationCapped {
			t.Errorf("L%d: DelegationCapped = false; the caller cannot tell the "+
				"level bound it apart from a failed requirement", l)
		}
		if !strings.Contains(res.Reason, "git-push-main") {
			t.Errorf("L%d: reason %q must name the operation", l, res.Reason)
		}
	}
}

func TestCheck_DelegationCeilingClearsAtFloor(t *testing.T) {
	for _, l := range []int{3, 4, 5} {
		res, err := Check(pushPolicy(ModeAuto), pushInput(level(l, true)))
		if err != nil {
			t.Fatalf("L%d: Check: %v", l, err)
		}
		if res.Mode != ModeAuto {
			t.Errorf("L%d: mode = %q, want %q — the policy's requires were met", l, res.Mode, ModeAuto)
		}
		if res.DelegationCapped {
			t.Errorf("L%d: DelegationCapped = true above the floor", l)
		}
	}
}

func TestCheck_DelegationCeilingBindsForceAuto(t *testing.T) {
	// force_auto is policy-authored. If it could lift the ceiling, the ceiling
	// would be advisory and a YAML edit would restore pushing at an authority
	// level nobody declared — the exact defect this exists to close.
	res, err := Check(pushPolicy(ModeForceAuto), pushInput(level(1, true)))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Mode != ModeConfirm || !res.DelegationCapped {
		t.Fatalf("force_auto at L1 = %+v, want a capped confirm", res)
	}
}

func TestCheck_DelegationNeverLoosens(t *testing.T) {
	// A high level must not turn a block into a push, nor rescue a rule whose
	// requires failed. The level is a ceiling, never a floor.
	res, err := Check(pushPolicy(ModeBlock), pushInput(level(5, true)))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Mode != ModeBlock {
		t.Errorf("block at L5 = %q, want block", res.Mode)
	}

	in := pushInput(level(5, true))
	in.CommittedByThisSession = false
	res, err = Check(pushPolicy(ModeAuto), in)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Mode != ModeConfirm {
		t.Errorf("failed requires at L5 = %q, want confirm", res.Mode)
	}
	if res.DelegationCapped {
		t.Error("DelegationCapped must not be set when the requirement itself failed")
	}
}

func TestCheck_NoDelegationSuppliedAppliesNoCeiling(t *testing.T) {
	// Library default. The command layer is what fails closed, by always
	// supplying a level (see cmdPolicyCheck).
	res, err := Check(pushPolicy(ModeAuto), pushInput(nil))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Mode != ModeAuto || res.DelegationCapped {
		t.Errorf("nil delegation = %+v, want an unmodified auto", res)
	}
}

func TestCheck_UnflooredOpIgnoresLevel(t *testing.T) {
	policy := &Policy{Version: 1, Rules: []Rule{
		{Op: "bead-close", Mode: ModeAuto},
		{Op: "*", Mode: ModeConfirm},
	}}
	res, err := Check(policy, CheckInput{Op: "bead-close", Delegation: level(0, true)})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Mode != ModeAuto {
		t.Errorf("bead-close at L0 = %q, want auto — it carries no delegation floor", res.Mode)
	}
}

func TestCheck_UndeclaredLevelSaysSo(t *testing.T) {
	res, err := Check(pushPolicy(ModeAuto), pushInput(level(autonomy.DefaultLevel, false)))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Mode != ModeConfirm {
		t.Fatalf("undeclared level = %q, want confirm", res.Mode)
	}
	if !strings.Contains(res.Reason, "undeclared") {
		t.Errorf("reason %q should distinguish an undeclared level from a chosen one", res.Reason)
	}
}
