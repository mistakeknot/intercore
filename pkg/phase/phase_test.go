package phase

import "testing"

func TestIsValid(t *testing.T) {
	// All 9 lifecycle phases are valid.
	for _, p := range DefaultChain {
		if !IsValid(p) {
			t.Errorf("IsValid(%q) = false, want true", p)
		}
	}
}

func TestIsValidRejectsOODARC(t *testing.T) {
	// OODARC phases are a different concept — not lifecycle phases.
	oodarc := []string{"observe", "orient", "decide", "act", "compound"}
	for _, p := range oodarc {
		if IsValid(p) {
			t.Errorf("IsValid(%q) = true, want false (OODARC, not lifecycle)", p)
		}
	}
}

func TestIsValidRejectsInvalid(t *testing.T) {
	invalid := []string{"", "plan-reviewed", "shipping", "foo", "BRAINSTORM"}
	for _, p := range invalid {
		if IsValid(p) {
			t.Errorf("IsValid(%q) = true, want false", p)
		}
	}
}

func TestDeprecatedAliases(t *testing.T) {
	if PlanReviewed != Planned {
		t.Errorf("PlanReviewed = %q, want %q", PlanReviewed, Planned)
	}
	if Shipping != Polish {
		t.Errorf("Shipping = %q, want %q", Shipping, Polish)
	}
}

func TestIsValidForChain(t *testing.T) {
	custom := []string{"alpha", "beta", "gamma"}
	if !IsValidForChain("beta", custom) {
		t.Error("IsValidForChain(beta, custom) = false, want true")
	}
	if IsValidForChain("delta", custom) {
		t.Error("IsValidForChain(delta, custom) = true, want false")
	}
}

func TestDefaultChainLength(t *testing.T) {
	if len(DefaultChain) != 9 {
		t.Errorf("DefaultChain has %d phases, want 9", len(DefaultChain))
	}
}

func TestDefaultChainOrder(t *testing.T) {
	want := []string{
		"brainstorm", "brainstorm-reviewed", "strategized", "planned",
		"executing", "review", "polish", "reflect", "done",
	}
	for i, p := range DefaultChain {
		if p != want[i] {
			t.Errorf("DefaultChain[%d] = %q, want %q", i, p, want[i])
		}
	}
}
