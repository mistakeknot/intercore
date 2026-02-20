package budget

// Budget is a multi-dimensional resource limit.
// Zero means "unlimited" (no constraint) for each field.
type Budget struct {
	TokenLimit   int64
	TimeLimitSec int64
	PhaseCount   int
}

// Meet returns the componentwise minimum (tropical semiring).
// Zero means unlimited: meet(0, x) = x, meet(x, 0) = x, meet(x, y) = min(x, y).
func (a Budget) Meet(b Budget) Budget {
	return Budget{
		TokenLimit:   minPositive(a.TokenLimit, b.TokenLimit),
		TimeLimitSec: minPositive(a.TimeLimitSec, b.TimeLimitSec),
		PhaseCount:   int(minPositive(int64(a.PhaseCount), int64(b.PhaseCount))),
	}
}

// Exceeded checks whether usage exceeds any non-zero dimension of this budget.
func (a Budget) Exceeded(tokens, elapsedSec int64, phases int) bool {
	if a.TokenLimit > 0 && tokens > a.TokenLimit {
		return true
	}
	if a.TimeLimitSec > 0 && elapsedSec > a.TimeLimitSec {
		return true
	}
	if a.PhaseCount > 0 && phases > a.PhaseCount {
		return true
	}
	return false
}

// minPositive returns the smaller of two values, treating 0 as unlimited.
func minPositive(a, b int64) int64 {
	if a == 0 {
		return b
	}
	if b == 0 {
		return a
	}
	if a < b {
		return a
	}
	return b
}
