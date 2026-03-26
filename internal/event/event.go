package event

import (
	"fmt"
	"time"
)

// Source identifies the origin subsystem.
const (
	SourcePhase        = "phase"
	SourceDispatch     = "dispatch"
	SourceInterspect   = "interspect"
	SourceDiscovery    = "discovery"
	SourceCoordination = "coordination"
	SourceReview       = "review"
	SourceIntent       = "intent"
)

// InterspectEvent represents a human correction or agent dispatch signal.
type InterspectEvent struct {
	ID             int64     `json:"id"`
	RunID          string    `json:"run_id,omitempty"`
	AgentName      string    `json:"agent_name"`
	EventType      string    `json:"event_type"`
	OverrideReason string    `json:"override_reason,omitempty"`
	ContextJSON    string    `json:"context_json,omitempty"`
	SessionID      string    `json:"session_id,omitempty"`
	ProjectDir     string    `json:"project_dir,omitempty"`
	Timestamp      time.Time `json:"timestamp"`
}

// ReviewEvent represents a disagreement resolution or execution defect from review/sprint.
type ReviewEvent struct {
	ID              int64     `json:"id"`
	RunID           string    `json:"run_id,omitempty"`
	FindingID       string    `json:"finding_id"`
	AgentsJSON      string    `json:"agents_json"`
	Resolution      string    `json:"resolution"`
	DismissalReason string    `json:"dismissal_reason,omitempty"`
	ChosenSeverity  string    `json:"chosen_severity"`
	Impact          string    `json:"impact"`
	EventType       string    `json:"event_type"`
	SessionID       string    `json:"session_id,omitempty"`
	ProjectDir      string    `json:"project_dir,omitempty"`
	Timestamp       time.Time `json:"timestamp"`
}

// Event is the unified event type for the intercore event bus.
type Event struct {
	ID        int64          `json:"id"`
	RunID     string         `json:"run_id"`
	Source    string         `json:"source" jsonschema:"enum=phase,enum=dispatch,enum=interspect,enum=discovery,enum=coordination,enum=review,enum=intent"` // origin subsystem — see contracts/events/README.md
	Type      string         `json:"type"`                                                                                                                  // "advance", "skip", "block", "status_change", etc.
	FromState string         `json:"from_state"`                                                                                                            // source-dependent: from_phase, from_status, owner, finding_id
	ToState   string         `json:"to_state"`                                                                                                              // source-dependent: to_phase, to_status, pattern, resolution
	Reason    string         `json:"reason,omitempty"`
	Envelope  *EventEnvelope `json:"envelope,omitempty"`
	Timestamp time.Time      `json:"timestamp"`
}

// validSources is the set of recognized event source values.
// Unexported to prevent external mutation — use Event.Validate() instead.
// NOTE: When adding a new Source* constant, add it here too.
var validSources = map[string]bool{
	SourcePhase:        true,
	SourceDispatch:     true,
	SourceInterspect:   true,
	SourceDiscovery:    true,
	SourceCoordination: true,
	SourceReview:       true,
	SourceIntent:       true,
}

// Validate checks that the event has a recognized Source value.
func (e *Event) Validate() error {
	if !validSources[e.Source] {
		return fmt.Errorf("unknown event source %q", e.Source)
	}
	return nil
}
