package phase

// OODARC roles — the per-turn behavioral loop an agent runs *within* each
// lifecycle phase. These mirror Skaffen's canonical definitions in
// os/Skaffen/internal/tool/tool.go (PhaseObserve/Orient/Decide/Act/Reflect/
// Compound). Keeping the string values identical to Skaffen's lets both
// runtimes share one vocabulary and prevents the two from drifting.
//
// This is an *annotation* layer over the lifecycle FSM, not a state change:
// the DefaultChain constants and the gate machinery are untouched. The map
// answers "which OODARC leg is an agent performing while a sprint sits in this
// lifecycle phase?" — used by host-surface labels (Clavain sprint/work phase
// headings) so they derive from one source of truth instead of being
// hand-maintained in Markdown.
const (
	RoleObserve  = "observe"
	RoleOrient   = "orient"
	RoleDecide   = "decide"
	RoleAct      = "act"
	RoleReflect  = "reflect"
	RoleCompound = "compound"

	// RoleReflectCompound marks the lifecycle phase that runs both the
	// Reflect and Compound legs back-to-back (extract the lesson, then
	// persist it). OODARC's last two legs collapse into one lifecycle phase.
	RoleReflectCompound = "reflect+compound"

	// RoleTerminal marks the end-of-loop phase (done) — no OODARC leg runs;
	// the loop has closed.
	RoleTerminal = "terminal"
)

// PhaseToOODARCRole maps each lifecycle phase in DefaultChain to the OODARC
// leg an agent predominantly performs while the sprint sits in that phase.
//
// Rationale per phase (see docs/brainstorms/2026-03-19-sprint-v2-lifecycle-redesign-brainstorm.md):
//   - brainstorm           → observe   (gather the problem space)
//   - brainstorm-reviewed  → observe   (quality-observe: read the review of what was gathered)
//   - strategized          → orient    (situate the gathered facts against the goal)
//   - planned              → decide    (commit to an approach)
//   - executing            → act       (carry out the plan)
//   - review               → observe   (quality-observe: read review findings on the work)
//   - polish               → act       (corrective action on findings)
//   - reflect              → reflect+compound (extract the lesson and persist it)
//   - done                 → terminal  (loop closed)
var PhaseToOODARCRole = map[string]string{
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

// OODARCRole returns the OODARC leg for a lifecycle phase, plus whether the
// phase is known. Unknown phases return ("", false) so callers can decide
// whether to fall back or surface a gap (rather than silently labeling
// something "observe").
func OODARCRole(lifecyclePhase string) (string, bool) {
	role, ok := PhaseToOODARCRole[lifecyclePhase]
	return role, ok
}
