// Package runner is the scheduler runner: a DB-driven loop that claims jobs and
// launches them through dispatch.Spawn. The execution code compiles only under
// the icsched_test build tag; the pure parts (classification, the in-flight
// map, the repair predicate and the path guard) are always built so they can be
// unit tested.
package runner

import "github.com/mistakeknot/intercore/internal/scheduler"

// Evidence is everything Classify reads. It is assembled from persisted rows
// only: the dispatch terminal, the verdict the dispatch recorded, the evidence
// class the runner wrote once into the attempt row, and the job's status.
type Evidence struct {
	// Cancelling is true when the job was cancelling or cancelled.
	Cancelling bool
	// DispatchPresent is false when the linked dispatch row is gone (pruned).
	DispatchPresent bool
	// Status is the dispatch status; only a terminal status is a result.
	Status string
	// FailureClass is the dispatch terminal's failure_class.
	FailureClass string
	// Verdict is the dispatch's verdict_status ("" when none).
	Verdict string
	// EvidenceClass is the attempt row's evidence_class: a normalized class,
	// "none", or "" when the evidence transaction has not run yet.
	EvidenceClass string
}

const (
	statusCompleted = "completed"
	statusFailed    = "failed"
	statusTimeout   = "timeout"
	statusCancelled = "cancelled"
)

func terminalDispatchStatus(s string) bool {
	switch s {
	case statusCompleted, statusFailed, statusTimeout, statusCancelled:
		return true
	}
	return false
}

func validVerdict(v string) bool {
	switch v {
	case "pass", "warn", "fail":
		return true
	}
	return false
}

// Classify maps persisted evidence to a scheduler outcome class. The first
// matching row of the precedence table wins. It is pure and is the only place a
// class is decided, so a crash anywhere after the evidence write reproduces the
// same answer.
func Classify(e Evidence) string {
	switch {
	case e.Cancelling || (e.DispatchPresent && e.Status == statusCancelled):
		return scheduler.ClassCancelled
	case e.DispatchPresent && e.Status == statusCompleted:
		return scheduler.ClassOK
	case e.DispatchPresent && e.FailureClass == "spawn_failed":
		return scheduler.ClassPrestart
	case e.DispatchPresent && e.FailureClass == "indeterminate":
		return scheduler.ClassIndeterminate
	case e.DispatchPresent && e.Status == statusTimeout:
		return scheduler.ClassTimeout
	case !e.DispatchPresent || !terminalDispatchStatus(e.Status):
		return scheduler.ClassIndeterminate
	case e.Status == statusFailed && !validVerdict(e.Verdict):
		return scheduler.ClassIndeterminate
	case e.Status == statusFailed && (e.EvidenceClass == "quota_exhausted" || e.EvidenceClass == "rate_limited"):
		return scheduler.ClassCapacity
	case e.Status == statusFailed && e.EvidenceClass == "none":
		return scheduler.ClassFailedUnclassified
	case e.Status == statusFailed && e.EvidenceClass != "":
		return scheduler.ClassAgentFailed
	}
	return scheduler.ClassUnknown
}

// NormalizeEvidenceClass turns a failure class read from a receipt into a
// stored evidence class: a lower-case identifier of 1 to 64 characters, or
// "none" for anything absent, unbound, malformed or unknown.
func NormalizeEvidenceClass(raw string) string {
	if raw == "" || raw == "none" || len(raw) > 64 {
		return "none"
	}
	for _, r := range raw {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') {
			return "none"
		}
	}
	return raw
}
