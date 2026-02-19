package event

import "time"

// Source identifies the origin subsystem.
const (
	SourcePhase    = "phase"
	SourceDispatch = "dispatch"
)

// Event is the unified event type for the intercore event bus.
type Event struct {
	ID        int64     `json:"id"`
	RunID     string    `json:"run_id"`
	Source    string    `json:"source"`     // "phase" or "dispatch"
	Type      string    `json:"type"`       // "advance", "skip", "block", "status_change", etc.
	FromState string    `json:"from_state"` // from_phase or from_status
	ToState   string    `json:"to_state"`   // to_phase or to_status
	Reason    string    `json:"reason,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}
