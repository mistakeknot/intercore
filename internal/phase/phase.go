package phase

import "time"

// Phase constants — the linear lifecycle progression.
const (
	PhaseBrainstorm         = "brainstorm"
	PhaseBrainstormReviewed = "brainstorm-reviewed"
	PhaseStrategized        = "strategized"
	PhasePlanned            = "planned"
	PhaseExecuting          = "executing"
	PhaseReview             = "review"
	PhasePolish             = "polish"
	PhaseDone               = "done"
)

// Run status constants.
const (
	StatusActive    = "active"
	StatusCompleted = "completed"
	StatusCancelled = "cancelled"
	StatusFailed    = "failed"
)

// Event type constants.
const (
	EventAdvance = "advance"
	EventSkip    = "skip"
	EventPause   = "pause"
	EventBlock   = "block"
	EventCancel  = "cancel"
	EventSet     = "set"
)

// Gate result constants.
const (
	GatePass = "pass"
	GateFail = "fail"
	GateWarn = "warn"
	GateNone = "none"
)

// Gate tier constants (from interphase enforcement levels).
const (
	TierHard = "hard"
	TierSoft = "soft"
	TierNone = "none"
)

// transitionTable defines the strict single-successor chain.
// Each phase maps to its one next phase.
var transitionTable = map[string]string{
	PhaseBrainstorm:         PhaseBrainstormReviewed,
	PhaseBrainstormReviewed: PhaseStrategized,
	PhaseStrategized:        PhasePlanned,
	PhasePlanned:            PhaseExecuting,
	PhaseExecuting:          PhaseReview,
	PhaseReview:             PhasePolish,
	PhasePolish:             PhaseDone,
}

// allPhases in order for iteration.
var allPhases = []string{
	PhaseBrainstorm,
	PhaseBrainstormReviewed,
	PhaseStrategized,
	PhasePlanned,
	PhaseExecuting,
	PhaseReview,
	PhasePolish,
	PhaseDone,
}

// complexityWhitelist maps complexity tiers to allowed phases.
// Tier 1 (trivial): 4 phases — brainstorm, planned, executing, done
// Tier 2 (small):   5 phases — adds brainstorm-reviewed
// Tier 3-5 (full):  all 8 phases
var complexityWhitelist = map[int]map[string]bool{
	1: {
		PhaseBrainstorm: true,
		PhasePlanned:    true,
		PhaseExecuting:  true,
		PhaseDone:       true,
	},
	2: {
		PhaseBrainstorm:         true,
		PhaseBrainstormReviewed: true,
		PhasePlanned:            true,
		PhaseExecuting:          true,
		PhaseDone:               true,
	},
}

// validTransitions is the expanded graph (includes skip + re-entry paths).
// Any transition not in this map is illegal.
var validTransitions = map[string]map[string]bool{
	PhaseBrainstorm: {
		PhaseBrainstormReviewed: true,
		PhasePlanned:            true, // skip for complexity 1
		PhaseExecuting:          true, // extreme skip
	},
	PhaseBrainstormReviewed: {
		PhaseStrategized: true,
		PhasePlanned:     true, // skip for complexity 2
		PhaseExecuting:   true,
	},
	PhaseStrategized: {
		PhasePlanned: true,
	},
	PhasePlanned: {
		PhaseExecuting: true,
	},
	PhaseExecuting: {
		PhaseReview: true,
		PhaseDone:   true, // skip for complexity 1-2
	},
	PhaseReview: {
		PhasePolish: true,
		PhaseDone:   true,
	},
	PhasePolish: {
		PhaseDone: true,
	},
}

// NextPhase returns the single successor of the given phase in the linear chain.
func NextPhase(current string) (string, error) {
	next, ok := transitionTable[current]
	if !ok {
		return "", ErrNoTransition
	}
	return next, nil
}

// ShouldSkip returns true if the given phase should be skipped at the given complexity.
func ShouldSkip(p string, complexity int) bool {
	if complexity >= 3 {
		return false // full lifecycle
	}
	whitelist, ok := complexityWhitelist[complexity]
	if !ok {
		return false // unknown complexity → full lifecycle
	}
	return !whitelist[p]
}

// NextRequiredPhase walks the transition table forward from current,
// skipping phases not in the whitelist for the given complexity.
// Returns the next phase the run should actually enter.
func NextRequiredPhase(current string, complexity int, forceFull bool) string {
	if forceFull {
		next, err := NextPhase(current)
		if err != nil {
			return PhaseDone
		}
		return next
	}

	p := current
	for {
		next, err := NextPhase(p)
		if err != nil {
			return PhaseDone
		}
		if !ShouldSkip(next, complexity) {
			return next
		}
		p = next
	}
}

// IsValidTransition checks if a direct transition from→to is legal.
func IsValidTransition(from, to string) bool {
	targets, ok := validTransitions[from]
	if !ok {
		return false
	}
	return targets[to]
}

// IsTerminalPhase returns true if the phase is a final state.
func IsTerminalPhase(p string) bool {
	return p == PhaseDone
}

// IsTerminalStatus returns true if the status is a final state.
func IsTerminalStatus(s string) bool {
	switch s {
	case StatusCompleted, StatusCancelled, StatusFailed:
		return true
	}
	return false
}

// Run represents a sprint run tracked in the database.
type Run struct {
	ID          string
	ProjectDir  string
	Goal        string
	Status      string
	Phase       string
	Complexity  int
	ForceFull   bool
	AutoAdvance bool
	CreatedAt   int64
	UpdatedAt   int64
	CompletedAt *int64
	ScopeID     *string
	Metadata    *string
}

// PhaseEvent represents an audit log entry for a phase transition.
type PhaseEvent struct {
	ID         int64
	RunID      string
	FromPhase  string
	ToPhase    string
	EventType  string
	GateResult *string
	GateTier   *string
	Reason     *string
	CreatedAt  int64
}

// strPtr returns a pointer to s.
func strPtr(s string) *string {
	return &s
}

// nowUnix returns the current Unix timestamp.
func nowUnix() int64 {
	return time.Now().Unix()
}
