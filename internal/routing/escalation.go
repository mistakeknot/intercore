package routing

import "github.com/mistakeknot/intercore/internal/capability"

// This file owns the capability *ordering* used by escalation's next-rung
// selection. It exists so that internal/dispatch no longer re-implements a
// hardcoded Claude ladder (the fd-architecture F1 P0): escalation now asks
// routing "what is the next-more-capable target above X?", giving the system
// one source of capability order instead of three.
//
// v1 preserves the historical Claude ladder behavior (sonnet -> opus -> fable,
// fable gated by the frontier window) so this refactor is behavior-preserving.
// The registry-vector upgrade (rank deployments by a capability axis for a
// given role, cross-vendor) is a drop-in replacement for NextRung's body once
// the §2 registry-ranking decisions land; the SEAM (dispatch calls routing)
// is the load-bearing change and is in place now.

// defaultLadder is the historical Claude capability ladder, low -> high.
// Owned here (not in dispatch) as the single source of ordering until the
// registry-vector ranking replaces it.
var defaultLadder = []string{"sonnet", "opus", "fable"}

// NextRung returns the next-more-capable model above currentModel on the
// capability ladder, and whether one exists. It is the single authority for
// "which model is more capable than X" that escalation consumes.
//
// Frontier (fable) is fail-closed: it is only offered when the frontier window
// is open (capability.FrontierWindowOpen); otherwise the ladder tops out at the
// rung below it. A currentModel not on the ladder is treated as base rung, so
// the first escalation steps to the lowest ladder rung above base.
func NextRung(currentModel string) (string, bool) {
	idx := -1
	for i, m := range defaultLadder {
		if m == currentModel {
			idx = i
			break
		}
	}
	// Off-ladder model (e.g. a codex ID): treat as below the base rung, so the
	// next rung is the first ladder entry.
	if idx == -1 {
		next := defaultLadder[0]
		if next == "fable" && !capability.FrontierWindowOpen() {
			return "", false
		}
		return next, true
	}
	if idx+1 >= len(defaultLadder) {
		return "", false // already at the top
	}
	next := defaultLadder[idx+1]
	if next == "fable" && !capability.FrontierWindowOpen() {
		// Frontier closed: opus is the effective ceiling. From opus there is
		// nowhere higher to go.
		return "", false
	}
	return next, true
}
