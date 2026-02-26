package coordination

import "time"

const (
	TypeFileReservation = "file_reservation"
	TypeNamedLock       = "named_lock"
	TypeWriteSet        = "write_set"
)

// Lock represents a coordination lock in the coordination_locks table.
type Lock struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Owner      string `json:"owner"`
	Scope      string `json:"scope"`
	Pattern    string `json:"pattern"`
	Exclusive  bool   `json:"exclusive"`
	Reason     string `json:"reason,omitempty"`
	TTLSeconds int    `json:"ttl_seconds,omitempty"`
	CreatedAt  int64  `json:"created_at"`
	ExpiresAt  *int64 `json:"expires_at,omitempty"`
	ReleasedAt *int64 `json:"released_at,omitempty"`
	DispatchID string `json:"dispatch_id,omitempty"`
	RunID      string `json:"run_id,omitempty"`
}

// ConflictInfo describes the lock that blocked a reserve attempt.
type ConflictInfo struct {
	BlockerID      string `json:"blocker_id"`
	BlockerOwner   string `json:"blocker_owner"`
	BlockerPattern string `json:"blocker_pattern"`
	BlockerReason  string `json:"blocker_reason,omitempty"`
}

// ReserveResult is the outcome of a Reserve call.
type ReserveResult struct {
	Lock     *Lock         `json:"lock,omitempty"`
	Conflict *ConflictInfo `json:"conflict,omitempty"`
}

// ListFilter controls what List() returns.
type ListFilter struct {
	Scope  string
	Owner  string
	Type   string
	Active bool // if true, only released_at IS NULL
}

// SweepResult summarizes what Sweep cleaned.
type SweepResult struct {
	Expired int `json:"expired"`
	Total   int `json:"total"`
}

// IsActive returns true if the lock is not released and not expired.
func (l *Lock) IsActive() bool {
	if l.ReleasedAt != nil {
		return false
	}
	if l.ExpiresAt != nil && *l.ExpiresAt < time.Now().Unix() {
		return false
	}
	return true
}
