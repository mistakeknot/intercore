package phase

import "testing"

// Every lifecycle phase in DefaultChain must have an OODARC role. A phase
// without one is a legibility gap — host-surface labels would have nothing to
// derive from and would silently fall back to an unlabeled heading.
func TestEveryDefaultChainPhaseHasRole(t *testing.T) {
	for _, p := range DefaultChain {
		role, ok := OODARCRole(p)
		if !ok {
			t.Errorf("OODARCRole(%q) missing — every DefaultChain phase needs a role", p)
			continue
		}
		if role == "" {
			t.Errorf("OODARCRole(%q) returned empty role", p)
		}
	}
}

// The map must not carry roles for phases that aren't in the lifecycle — a
// stray key means the map and the FSM have drifted.
func TestNoExtraRoleKeys(t *testing.T) {
	for p := range PhaseToOODARCRole {
		if !IsValid(p) {
			t.Errorf("PhaseToOODARCRole has key %q which is not a valid lifecycle phase", p)
		}
	}
}

// Role string values must match Skaffen's canonical OODARC vocabulary
// (os/Skaffen/internal/tool/tool.go). If these drift, the two runtimes label
// the same work differently. The terminal/reflect+compound values are
// intercore-local composites (Skaffen has no "done" phase), so they're
// excluded from the shared-vocabulary check.
func TestRolesUseSkaffenVocabulary(t *testing.T) {
	skaffenVocab := map[string]bool{
		RoleObserve:  true,
		RoleOrient:   true,
		RoleDecide:   true,
		RoleAct:      true,
		RoleReflect:  true,
		RoleCompound: true,
	}
	localComposites := map[string]bool{
		RoleReflectCompound: true,
		RoleTerminal:        true,
	}
	for p, role := range PhaseToOODARCRole {
		if skaffenVocab[role] || localComposites[role] {
			continue
		}
		t.Errorf("PhaseToOODARCRole[%q] = %q is neither a Skaffen OODARC leg nor a known local composite", p, role)
	}
}

func TestOODARCRoleUnknownPhase(t *testing.T) {
	if role, ok := OODARCRole("not-a-phase"); ok || role != "" {
		t.Errorf("OODARCRole(unknown) = (%q, %v), want (\"\", false)", role, ok)
	}
}

// Spot-check the conceptually load-bearing mappings: the two quality-observe
// checkpoints both fold to observe, and reflect carries both final legs.
func TestKeyRoleMappings(t *testing.T) {
	cases := map[string]string{
		Brainstorm:         RoleObserve,
		BrainstormReviewed: RoleObserve,
		Strategized:        RoleOrient,
		Planned:            RoleDecide,
		Executing:          RoleAct,
		Review:             RoleObserve,
		Polish:             RoleAct,
		Reflect:            RoleReflectCompound,
		Done:               RoleTerminal,
	}
	for p, want := range cases {
		if got, _ := OODARCRole(p); got != want {
			t.Errorf("OODARCRole(%q) = %q, want %q", p, got, want)
		}
	}
}
