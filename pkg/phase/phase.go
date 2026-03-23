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
