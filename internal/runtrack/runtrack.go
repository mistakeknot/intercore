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

// CodeRollbackEntry represents an artifact with dispatch metadata for code rollback queries.
type CodeRollbackEntry struct {
	DispatchID   *string `json:"dispatch_id"`
	DispatchName *string `json:"dispatch_name"`
	Phase        string  `json:"phase"`
	Path         string  `json:"path"`
	ContentHash  *string `json:"content_hash"`
	Type         string  `json:"type"`
	Status       string  `json:"status"`
}

// Artifact represents a file produced during a run.
type Artifact struct {
	ID          string
	RunID       string
	Phase       string
	Path        string
	Type        string
	ContentHash *string
	DispatchID  *string
	Status      *string
	CreatedAt   int64
}
