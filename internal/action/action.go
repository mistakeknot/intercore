package action

// Action represents a phase-triggered action for a run.
type Action struct {
	ID         int64
	RunID      string
	Phase      string
	ActionType string  // "command", "spawn", "hook"
	Command    string  // e.g., "/clavain:work", "/interflux:flux-drive"
	Args       *string // JSON array, may contain ${artifact:<type>} placeholders
	Mode       string  // "interactive", "autonomous", "both"
	Priority   int     // ordering when multiple actions per phase
	CreatedAt  int64
	UpdatedAt  int64
}

// ActionUpdate contains fields that can be updated on an existing action.
type ActionUpdate struct {
	Args     *string
	Mode     *string
	Priority *int
}
