package budget

import "testing"

func TestMeet(t *testing.T) {
	tests := []struct {
		name string
		a, b Budget
		want Budget
	}{
		{
			name: "identity with zero",
			a:    Budget{TokenLimit: 1000, TimeLimitSec: 60, PhaseCount: 5},
			b:    Budget{},
			want: Budget{TokenLimit: 1000, TimeLimitSec: 60, PhaseCount: 5},
		},
		{
			name: "symmetric",
			a:    Budget{},
			b:    Budget{TokenLimit: 1000, TimeLimitSec: 60, PhaseCount: 5},
			want: Budget{TokenLimit: 1000, TimeLimitSec: 60, PhaseCount: 5},
		},
		{
			name: "tighter wins",
			a:    Budget{TokenLimit: 1000, TimeLimitSec: 120, PhaseCount: 5},
			b:    Budget{TokenLimit: 500, TimeLimitSec: 60, PhaseCount: 10},
			want: Budget{TokenLimit: 500, TimeLimitSec: 60, PhaseCount: 5},
		},
		{
			name: "all zero",
			a:    Budget{},
			b:    Budget{},
			want: Budget{},
		},
		{
			name: "partial overlap",
			a:    Budget{TokenLimit: 1000},
			b:    Budget{TimeLimitSec: 30},
			want: Budget{TokenLimit: 1000, TimeLimitSec: 30},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.a.Meet(tt.b)
			if got != tt.want {
				t.Errorf("Meet() = %+v, want %+v", got, tt.want)
			}
			// Verify symmetry: a.Meet(b) == b.Meet(a)
			rev := tt.b.Meet(tt.a)
			if rev != got {
				t.Errorf("Meet is not symmetric: a.Meet(b) = %+v, b.Meet(a) = %+v", got, rev)
			}
		})
	}
}

func TestExceeded(t *testing.T) {
	tests := []struct {
		name       string
		budget     Budget
		tokens     int64
		elapsedSec int64
		phases     int
		want       bool
	}{
		{
			name:       "within budget",
			budget:     Budget{TokenLimit: 1000, TimeLimitSec: 60, PhaseCount: 5},
			tokens:     500,
			elapsedSec: 30,
			phases:     3,
			want:       false,
		},
		{
			name:       "token exceeded",
			budget:     Budget{TokenLimit: 1000},
			tokens:     1001,
			elapsedSec: 0,
			phases:     0,
			want:       true,
		},
		{
			name:       "time exceeded",
			budget:     Budget{TimeLimitSec: 60},
			tokens:     0,
			elapsedSec: 61,
			phases:     0,
			want:       true,
		},
		{
			name:       "phase exceeded",
			budget:     Budget{PhaseCount: 3},
			tokens:     0,
			elapsedSec: 0,
			phases:     4,
			want:       true,
		},
		{
			name:       "unlimited ignores all",
			budget:     Budget{},
			tokens:     999999,
			elapsedSec: 999999,
			phases:     999999,
			want:       false,
		},
		{
			name:       "at exact limit not exceeded",
			budget:     Budget{TokenLimit: 1000, TimeLimitSec: 60, PhaseCount: 5},
			tokens:     1000,
			elapsedSec: 60,
			phases:     5,
			want:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.budget.Exceeded(tt.tokens, tt.elapsedSec, tt.phases)
			if got != tt.want {
				t.Errorf("Exceeded(%d, %d, %d) = %v, want %v",
					tt.tokens, tt.elapsedSec, tt.phases, got, tt.want)
			}
		})
	}
}
