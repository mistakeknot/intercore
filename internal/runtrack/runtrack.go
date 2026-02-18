package runtrack

// Status constants for agent lifecycle.
const (
	StatusActive    = "active"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
)

// Agent represents an agent instance within a run.
type Agent struct {
	ID         string
	RunID      string
	AgentType  string
	Name       *string
	Status     string
	DispatchID *string
	CreatedAt  int64
	UpdatedAt  int64
}

// IsTerminal returns true if the agent is in a final state.
func (a *Agent) IsTerminal() bool {
	switch a.Status {
	case StatusCompleted, StatusFailed:
		return true
	}
	return false
}

// Artifact represents a file produced during a run.
type Artifact struct {
	ID        string
	RunID     string
	Phase     string
	Path      string
	Type      string
	CreatedAt int64
}
