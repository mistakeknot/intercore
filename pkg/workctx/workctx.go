package workctx

// WorkContext is the (bead, run, session) trinity that identifies a unit of work.
// This type crosses module boundaries — Skaffen, Clavain, and plugins all import it.
// Partial contexts are valid (e.g., session-only before a bead is claimed).
type WorkContext struct {
	BeadID    string `json:"bead_id"`
	RunID     string `json:"run_id"`
	SessionID string `json:"session_id"`
}

// IsZero returns true if all fields are empty.
func (wc WorkContext) IsZero() bool {
	return wc.BeadID == "" && wc.RunID == "" && wc.SessionID == ""
}
