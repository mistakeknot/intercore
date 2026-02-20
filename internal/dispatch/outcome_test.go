package dispatch

import "testing"

func TestOutcomeString(t *testing.T) {
	tests := []struct {
		outcome Outcome
		want    string
	}{
		{OutcomeSuccess, "success"},
		{OutcomeError, "error"},
		{OutcomeCancelled, "cancelled"},
		{OutcomeTimeout, "timeout"},
		{Outcome(99), "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := tt.outcome.String(); got != tt.want {
				t.Errorf("Outcome(%d).String() = %q, want %q", tt.outcome, got, tt.want)
			}
		})
	}
}

func TestWorst(t *testing.T) {
	tests := []struct {
		name string
		a, b Outcome
		want Outcome
	}{
		{"success vs error", OutcomeSuccess, OutcomeError, OutcomeError},
		{"error vs timeout", OutcomeError, OutcomeTimeout, OutcomeTimeout},
		{"cancelled vs error", OutcomeCancelled, OutcomeError, OutcomeCancelled},
		{"timeout vs timeout", OutcomeTimeout, OutcomeTimeout, OutcomeTimeout},
		{"success vs success", OutcomeSuccess, OutcomeSuccess, OutcomeSuccess},
		{"symmetric", OutcomeTimeout, OutcomeSuccess, OutcomeTimeout},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.a.Worst(tt.b)
			if got != tt.want {
				t.Errorf("Worst() = %v, want %v", got, tt.want)
			}
			// Verify commutativity
			rev := tt.b.Worst(tt.a)
			if rev != got {
				t.Errorf("Worst is not commutative: a.Worst(b) = %v, b.Worst(a) = %v", got, rev)
			}
		})
	}
}

func TestAggregate(t *testing.T) {
	tests := []struct {
		name     string
		outcomes []Outcome
		want     Outcome
	}{
		{"empty slice", nil, OutcomeSuccess},
		{"single success", []Outcome{OutcomeSuccess}, OutcomeSuccess},
		{"single error", []Outcome{OutcomeError}, OutcomeError},
		{"mixed outcomes", []Outcome{OutcomeSuccess, OutcomeError, OutcomeCancelled}, OutcomeCancelled},
		{"timeout dominates", []Outcome{OutcomeSuccess, OutcomeTimeout, OutcomeError}, OutcomeTimeout},
		{"all success", []Outcome{OutcomeSuccess, OutcomeSuccess, OutcomeSuccess}, OutcomeSuccess},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Aggregate(tt.outcomes)
			if got != tt.want {
				t.Errorf("Aggregate() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFromStatus(t *testing.T) {
	tests := []struct {
		status string
		want   Outcome
	}{
		{StatusCompleted, OutcomeSuccess},
		{StatusFailed, OutcomeError},
		{StatusCancelled, OutcomeCancelled},
		{StatusTimeout, OutcomeTimeout},
		{StatusSpawned, OutcomeError},  // non-terminal fallback
		{StatusRunning, OutcomeError},  // non-terminal fallback
		{"garbage", OutcomeError},      // unknown fallback
	}

	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			got := FromStatus(tt.status)
			if got != tt.want {
				t.Errorf("FromStatus(%q) = %v, want %v", tt.status, got, tt.want)
			}
		})
	}
}
