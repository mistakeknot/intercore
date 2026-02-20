package dispatch

// Outcome is a four-valued terminal result with severity ordering.
// Severity: Success(0) < Error(1) < Cancelled(2) < Timeout(3).
type Outcome int

const (
	OutcomeSuccess   Outcome = 0
	OutcomeError     Outcome = 1
	OutcomeCancelled Outcome = 2
	OutcomeTimeout   Outcome = 3
)

// String returns the lowercase name of the outcome.
func (o Outcome) String() string {
	switch o {
	case OutcomeSuccess:
		return "success"
	case OutcomeError:
		return "error"
	case OutcomeCancelled:
		return "cancelled"
	case OutcomeTimeout:
		return "timeout"
	default:
		return "unknown"
	}
}

// Worst returns the outcome with higher severity (max).
func (a Outcome) Worst(b Outcome) Outcome {
	if b > a {
		return b
	}
	return a
}

// Aggregate returns the worst outcome across a slice.
// An empty slice returns OutcomeSuccess (vacuous truth: no failures).
func Aggregate(outcomes []Outcome) Outcome {
	worst := OutcomeSuccess
	for _, o := range outcomes {
		if o > worst {
			worst = o
		}
	}
	return worst
}

// FromStatus maps a dispatch status string to an Outcome.
// Non-terminal statuses (spawned, running) map to OutcomeError as a safe fallback.
func FromStatus(status string) Outcome {
	switch status {
	case StatusCompleted:
		return OutcomeSuccess
	case StatusFailed:
		return OutcomeError
	case StatusCancelled:
		return OutcomeCancelled
	case StatusTimeout:
		return OutcomeTimeout
	default:
		return OutcomeError
	}
}
