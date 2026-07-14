// Package capability holds cross-cutting capability-gating primitives shared
// by the routing mechanism and the dispatch/escalation subsystem.
//
// It exists to give a single home to logic that both packages need but that
// neither should own (and that must not drift between them). Its first
// resident is the frontier (fable) availability window, which was previously
// implemented as byte-identical copies in internal/routing (fableWindowOpen)
// and internal/dispatch (fableEscalationOpen). Keeping it here is a leaf
// dependency (imports only the standard library), so both callers can share
// it without internal/dispatch taking on internal/routing (that coupling is
// the deliberate Phase-2 escalation refactor, tracked separately).
package capability

import "os"

// frontierEnvVar gates access to the frontier (fable) tier. Documented as a
// named constant so the availability rule has one authoritative definition.
const frontierEnvVar = "CLAVAIN_FABLE_AVAILABLE"

// FrontierWindowOpen reports whether the frontier (fable) window is open.
//
// Fail-closed: the window is open only when the environment explicitly opts in
// with CLAVAIN_FABLE_AVAILABLE=1. Any other value (unset, "0", "true", "") is
// treated as closed. This is the single source of truth for frontier-tier
// availability across routing and escalation; do not reintroduce a local copy.
func FrontierWindowOpen() bool {
	return os.Getenv(frontierEnvVar) == "1"
}
