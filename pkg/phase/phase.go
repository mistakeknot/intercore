// Package phase exports lifecycle phase constants for cross-module use.
//
// Lifecycle phases (brainstorm → strategized → planned → executing → ...)
// represent where a sprint is in its progression. They are distinct from
// OODARC phases (observe/orient/decide/act/reflect/compound), which represent
// the agent's per-turn behavioral loop. OODARC happens *within* each lifecycle
// phase. See os/Skaffen/internal/tool/tool.go for OODARC definitions.
package phase

// Lifecycle phase constants — the sprint-level progression.
const (
	Brainstorm         = "brainstorm"
	BrainstormReviewed = "brainstorm-reviewed"
	Strategized        = "strategized"
	Planned            = "planned"
	Executing          = "executing"
	Review             = "review"
	Polish             = "polish"
	Reflect            = "reflect"
	Done               = "done"
)

// DefaultChain is the 9-phase Clavain lifecycle.
// Used when a run has no explicit phases column (NULL in DB).
var DefaultChain = []string{
	Brainstorm,
	BrainstormReviewed,
	Strategized,
	Planned,
	Executing,
	Review,
	Polish,
	Reflect,
	Done,
}

// Deprecated: use Planned. Alias for lib-sprint.sh phases_json compatibility.
// Remove after 2026-06-01.
const PlanReviewed = Planned

// Deprecated: use Polish. Alias for lib-sprint.sh phases_json compatibility.
// Remove after 2026-06-01.
const Shipping = Polish

// Legacy string values for backward compatibility with DB records.
// Clavain CLI's switch cases use these until ic migrate phases runs.
// Do NOT change these values — they match what's stored in IC databases.
const (
	LegacyPlanReviewed = "plan-reviewed" // DB value; canonical is Planned ("planned")
	LegacyShipping     = "shipping"      // DB value; canonical is Polish ("polish")
)

// GateCalibrationKey returns the canonical map key for calibrated tier lookups.
// Used by signal extraction, calibration command, and runtime integration.
func GateCalibrationKey(checkType, fromPhase, toPhase string) string {
	return checkType + ":" + fromPhase + "→" + toPhase
}

// IsValid returns true if p is in DefaultChain.
func IsValid(p string) bool {
	return IsValidForChain(p, DefaultChain)
}

// IsValidForChain returns true if p is a member of the given chain.
func IsValidForChain(p string, chain []string) bool {
	for _, c := range chain {
		if c == p {
			return true
		}
	}
	return false
}

// ModelTier returns a numeric tier for model comparison.
// Higher tier = more capable model. Returns 0 for unknown.
func ModelTier(model string) int {
	switch model {
	case "haiku":
		return 1
	case "sonnet":
		return 2
	case "opus":
		return 3
	default:
		return 0
	}
}
