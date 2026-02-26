package event

import "time"

// Source identifies the origin subsystem.
const (
	SourcePhase        = "phase"
	SourceDispatch     = "dispatch"
	SourceInterspect   = "interspect"
	SourceDiscovery    = "discovery"
	SourceCoordination = "coordination"
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

// Event is the unified event type for the intercore event bus.
type Event struct {
	ID        int64     `json:"id"`
	RunID     string    `json:"run_id"`
	Source    string    `json:"source"`     // "phase", "dispatch", or "discovery"
	Type      string    `json:"type"`       // "advance", "skip", "block", "status_change", etc.
	FromState string    `json:"from_state"` // from_phase or from_status
	ToState   string    `json:"to_state"`   // to_phase or to_status
	Reason    string    `json:"reason,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}
