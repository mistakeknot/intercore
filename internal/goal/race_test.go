package goal

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

// TestAcquireClose_TwoSessionRace demonstrates the double-witness prevention
// the melange flagged as source-read-only (f-001/f-025): N concurrent
// sessions race to close one goal; exactly one may hold the lease.
// Run with -race.
func TestAcquireClose_TwoSessionRace(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	id := mkGoal(t, s)

	const n = 16
	var wins, held int64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.AcquireClose(ctx, id, "run", "owner", 3600)
			switch {
			case err == nil:
				atomic.AddInt64(&wins, 1)
			case errors.Is(err, ErrLeaseHeld):
				atomic.AddInt64(&held, 1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if wins != 1 {
		t.Errorf("winners = %d, want exactly 1 (held=%d)", wins, held)
	}
	if wins+held != n {
		t.Errorf("wins+held = %d, want %d", wins+held, n)
	}
}
