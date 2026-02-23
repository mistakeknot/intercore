package phase

import (
	"encoding/json"
	"fmt"
	"time"
)

// Phase constants — the linear lifecycle progression.
const (
	PhaseBrainstorm         = "brainstorm"
	PhaseBrainstormReviewed = "brainstorm-reviewed"
	PhaseStrategized        = "strategized"
	PhasePlanned            = "planned"
	PhaseExecuting          = "executing"
	PhaseReview             = "review"
	PhasePolish             = "polish"
	PhaseReflect            = "reflect"
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
	EventSet      = "set"
	EventRollback = "rollback"
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

// IsTerminalStatus returns true if the status is a final state.
func IsTerminalStatus(s string) bool {
	switch s {
	case StatusCompleted, StatusCancelled, StatusFailed:
		return true
	}
	return false
}

// DefaultPhaseChain is the 9-phase Clavain lifecycle.
// Used when a run has no explicit phases column (NULL in DB).
var DefaultPhaseChain = []string{
	PhaseBrainstorm,
	PhaseBrainstormReviewed,
	PhaseStrategized,
	PhasePlanned,
	PhaseExecuting,
	PhaseReview,
	PhasePolish,
	PhaseReflect,
	PhaseDone,
}

// ParsePhaseChain parses and validates a JSON phase chain.
// Returns error if: not valid JSON array, fewer than 2 phases, or contains duplicates.
func ParsePhaseChain(jsonStr string) ([]string, error) {
	if jsonStr == "" {
		return nil, fmt.Errorf("parse phase chain: empty string")
	}
	var chain []string
	if err := json.Unmarshal([]byte(jsonStr), &chain); err != nil {
		return nil, fmt.Errorf("parse phase chain: %w", err)
	}
	if len(chain) < 2 {
		return nil, fmt.Errorf("parse phase chain: need at least 2 phases, got %d", len(chain))
	}
	seen := make(map[string]bool, len(chain))
	for _, p := range chain {
		if p == "" || !isValidPhaseName(p) {
			return nil, fmt.Errorf("parse phase chain: invalid phase name %q (must match [a-zA-Z0-9_-]+)", p)
		}
		if seen[p] {
			return nil, fmt.Errorf("parse phase chain: duplicate phase %q", p)
		}
		seen[p] = true
	}
	return chain, nil
}

// isValidPhaseName checks that a phase name contains only alphanumeric, hyphen, or underscore.
func isValidPhaseName(s string) bool {
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// ChainNextPhase returns the next phase in the chain after current.
func ChainNextPhase(chain []string, current string) (string, error) {
	for i, p := range chain {
		if p == current {
			if i+1 >= len(chain) {
				return "", ErrNoTransition
			}
			return chain[i+1], nil
		}
	}
	return "", fmt.Errorf("phase %q not found in chain", current)
}

// ChainIsValidTransition checks if from→to is a forward transition in the chain.
func ChainIsValidTransition(chain []string, from, to string) bool {
	fromIdx := -1
	toIdx := -1
	for i, p := range chain {
		if p == from {
			fromIdx = i
		}
		if p == to {
			toIdx = i
		}
	}
	return fromIdx >= 0 && toIdx > fromIdx
}

// ChainIsTerminal returns true if phase is the last in the chain.
func ChainIsTerminal(chain []string, p string) bool {
	return len(chain) > 0 && chain[len(chain)-1] == p
}

// ChainContains returns true if the chain contains the given phase.
func ChainContains(chain []string, p string) bool {
	for _, cp := range chain {
		if cp == p {
			return true
		}
	}
	return false
}

// ChainPhaseIndex returns the index of phase in chain, or -1 if not found.
func ChainPhaseIndex(chain []string, p string) int {
	for i, cp := range chain {
		if cp == p {
			return i
		}
	}
	return -1
}

// ChainPhasesBetween returns the phases strictly after from up to and including to.
// Returns nil if from is not before to in the chain or either phase is not found.
func ChainPhasesBetween(chain []string, from, to string) []string {
	fromIdx := ChainPhaseIndex(chain, from)
	toIdx := ChainPhaseIndex(chain, to)
	if fromIdx < 0 || toIdx < 0 || fromIdx >= toIdx {
		return nil
	}
	result := make([]string, 0, toIdx-fromIdx)
	for i := fromIdx + 1; i <= toIdx; i++ {
		result = append(result, chain[i])
	}
	return result
}

// ResolveChain returns the run's explicit chain or DefaultPhaseChain if nil.
func ResolveChain(r *Run) []string {
	if r.Phases != nil {
		return r.Phases
	}
	return DefaultPhaseChain
}

// Run represents a sprint run tracked in the database.
type Run struct {
	ID            string
	ProjectDir    string
	Goal          string
	Status        string
	Phase         string
	Complexity    int
	ForceFull     bool
	AutoAdvance   bool
	CreatedAt     int64
	UpdatedAt     int64
	CompletedAt   *int64
	ScopeID       *string
	Metadata      *string
	Phases        []string // parsed from JSON; nil = legacy chain
	TokenBudget   *int64
	BudgetWarnPct int
	ParentRunID    *string
	MaxDispatches  int
	BudgetEnforce  bool
	MaxAgents      int
	GateRules      map[string][]SpecGateRule // parsed from JSON; nil = use defaults
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
