// Package handoff provides a structured YAML handoff format for context
// preservation between agent sessions.
//
// A handoff is a compact (~400 tokens) representation of session state
// that enables continuity across session rotations, context exhaustion,
// and multi-agent coordination.
//
// Required fields: Goal (what was accomplished) and Now (what to do next).
//
// Inspired by research/ntm/internal/handoff/types.go, adapted for
// Intercore's dispatch and interlock integration.
package handoff

import (
	"fmt"
	"time"

	"encoding/json"
)

// Version tracks the format version for backwards compatibility.
const Version = "1.0"

// Status constants for handoff completion state.
const (
	StatusComplete = "complete"
	StatusPartial  = "partial"
	StatusBlocked  = "blocked"
)

// Outcome constants for session result classification.
const (
	OutcomeSucceeded    = "succeeded"
	OutcomePartialPlus  = "partial_plus"  // Some progress
	OutcomePartialMinus = "partial_minus" // Little progress
	OutcomeFailed       = "failed"
)

// Handoff represents a complete context handoff between sessions.
type Handoff struct {
	// Metadata
	Version   string    `json:"version"`
	SessionID string    `json:"session_id"`
	Date      string    `json:"date"`
	CreatedAt time.Time `json:"created_at"`

	// Status
	Status  string `json:"status"`
	Outcome string `json:"outcome"`

	// Core fields (REQUIRED)
	Goal string `json:"goal"` // What this session accomplished
	Now  string `json:"now"`  // What next session should do first
	Test string `json:"test"` // Command to verify this work

	// Work tracking
	Tasks []TaskRecord `json:"tasks,omitempty"`

	// Context for future sessions
	Blockers  []string          `json:"blockers,omitempty"`
	Questions []string          `json:"questions,omitempty"`
	Decisions map[string]string `json:"decisions,omitempty"`
	Findings  map[string]string `json:"findings,omitempty"`

	// Pattern observations
	Worked []string `json:"worked,omitempty"`
	Failed []string `json:"failed,omitempty"`

	// File tracking
	Files FileChanges `json:"files,omitempty"`

	// Agent info
	AgentID   string `json:"agent_id,omitempty"`
	AgentType string `json:"agent_type,omitempty"` // claude, codex, gemini

	// Token context at time of handoff
	Tokens TokenContext `json:"tokens,omitempty"`

	// Reservation transfer instructions
	ReservationTransfer *ReservationTransfer `json:"reservation_transfer,omitempty"`

	// Integration references
	DispatchID string   `json:"dispatch_id,omitempty"`
	RunID      string   `json:"run_id,omitempty"`
	BeadIDs    []string `json:"bead_ids,omitempty"`
}

// TaskRecord represents a completed task with associated file changes.
type TaskRecord struct {
	Task  string   `json:"task"`
	Files []string `json:"files,omitempty"`
}

// FileChanges tracks file modifications during a session.
type FileChanges struct {
	Created  []string `json:"created,omitempty"`
	Modified []string `json:"modified,omitempty"`
	Deleted  []string `json:"deleted,omitempty"`
}

// TokenContext captures token usage at handoff time.
type TokenContext struct {
	Used       int     `json:"used"`
	Max        int     `json:"max"`
	Percentage float64 `json:"percentage"`
}

// ReservationTransfer captures file reservation state for handoff.
type ReservationTransfer struct {
	FromAgent    string                `json:"from_agent"`
	ProjectKey   string                `json:"project_key"`
	TTLSeconds   int                   `json:"ttl_seconds"`
	GracePeriod  int                   `json:"grace_period_seconds"`
	Reservations []ReservationSnapshot `json:"reservations"`
}

// ReservationSnapshot captures a single reservation.
type ReservationSnapshot struct {
	PathPattern string `json:"path_pattern"`
	Exclusive   bool   `json:"exclusive"`
	Reason      string `json:"reason"`
}

// New creates a new handoff with required fields.
func New(goal, now string) *Handoff {
	now_time := time.Now().UTC()
	return &Handoff{
		Version:   Version,
		Date:      now_time.Format("2006-01-02"),
		CreatedAt: now_time,
		Status:    StatusComplete,
		Outcome:   OutcomeSucceeded,
		Goal:      goal,
		Now:       now,
	}
}

// Validate checks that the handoff has all required fields and valid values.
func (h *Handoff) Validate() error {
	if h.Goal == "" {
		return fmt.Errorf("handoff: goal is required")
	}
	if h.Now == "" {
		return fmt.Errorf("handoff: now is required")
	}
	if h.Status != "" && h.Status != StatusComplete && h.Status != StatusPartial && h.Status != StatusBlocked {
		return fmt.Errorf("handoff: invalid status %q (must be complete, partial, or blocked)", h.Status)
	}
	if h.Outcome != "" && h.Outcome != OutcomeSucceeded && h.Outcome != OutcomePartialPlus &&
		h.Outcome != OutcomePartialMinus && h.Outcome != OutcomeFailed {
		return fmt.Errorf("handoff: invalid outcome %q", h.Outcome)
	}
	if h.Tokens.Used < 0 {
		return fmt.Errorf("handoff: tokens.used cannot be negative")
	}
	if h.Tokens.Max < 0 {
		return fmt.Errorf("handoff: tokens.max cannot be negative")
	}
	if h.Tokens.Percentage < 0 || h.Tokens.Percentage > 100 {
		return fmt.Errorf("handoff: tokens.percentage must be 0-100, got %.1f", h.Tokens.Percentage)
	}
	return nil
}

// SetTokens sets the token context, computing percentage automatically.
func (h *Handoff) SetTokens(used, max int) *Handoff {
	if used < 0 {
		used = 0
	}
	if max < 0 {
		max = 0
	}
	if max > 0 && used > max {
		used = max
	}
	h.Tokens = TokenContext{
		Used: used,
		Max:  max,
	}
	if max > 0 {
		h.Tokens.Percentage = float64(used) / float64(max) * 100
	}
	return h
}

// SetAgent sets agent identification fields.
func (h *Handoff) SetAgent(agentID, agentType string) *Handoff {
	h.AgentID = agentID
	h.AgentType = agentType
	return h
}

// SetDispatch links the handoff to an intercore dispatch and run.
func (h *Handoff) SetDispatch(dispatchID, runID string) *Handoff {
	h.DispatchID = dispatchID
	h.RunID = runID
	return h
}

// AddTask records a completed task.
func (h *Handoff) AddTask(task string, files ...string) *Handoff {
	h.Tasks = append(h.Tasks, TaskRecord{Task: task, Files: files})
	return h
}

// AddFileChanges records file modifications.
func (h *Handoff) AddFileChanges(created, modified, deleted []string) *Handoff {
	h.Files.Created = append(h.Files.Created, created...)
	h.Files.Modified = append(h.Files.Modified, modified...)
	h.Files.Deleted = append(h.Files.Deleted, deleted...)
	return h
}

// HasChanges returns true if any file changes were recorded.
func (h *Handoff) HasChanges() bool {
	return len(h.Files.Created)+len(h.Files.Modified)+len(h.Files.Deleted) > 0
}

// TotalChanges returns the total count of file changes.
func (h *Handoff) TotalChanges() int {
	return len(h.Files.Created) + len(h.Files.Modified) + len(h.Files.Deleted)
}

// IsComplete returns true if the handoff is marked complete.
func (h *Handoff) IsComplete() bool {
	return h.Status == StatusComplete
}

// IsBlocked returns true if the handoff is marked blocked.
func (h *Handoff) IsBlocked() bool {
	return h.Status == StatusBlocked
}

// MarshalJSON produces the JSON representation.
func (h *Handoff) MarshalJSON() ([]byte, error) {
	type Alias Handoff
	return json.Marshal((*Alias)(h))
}

// UnmarshalJSON parses JSON into a Handoff.
func (h *Handoff) UnmarshalJSON(data []byte) error {
	type Alias Handoff
	return json.Unmarshal(data, (*Alias)(h))
}
