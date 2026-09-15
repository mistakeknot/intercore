package coordination

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

// Transfer reads the locks it moves and then writes. In a deferred transaction,
// a commit by another process between that read and the write makes SQLite
// refuse the write at once (SQLITE_BUSY_SNAPSHOT) instead of waiting out the busy
// timeout. Coordination writes must begin immediate so that they wait instead.
func TestCoordinationBeginsImmediate(t *testing.T) {
	const (
		handles = 4
		rounds  = 40
	)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	stores := make([]*Store, handles)
	for i := range stores {
		stores[i] = setupTestStoreAt(t, path)
	}
	const scope = "/tmp/project"

	for r := 0; r < rounds; r++ {
		start := make(chan struct{})
		errs := make([]error, handles)
		var wg sync.WaitGroup
		for i, s := range stores {
			wg.Add(1)
			go func(i int, s *Store) {
				defer wg.Done()
				<-start
				if i%2 == 0 {
					_, err := s.Transfer(ctx, "agent-a", "agent-b", scope, false)
					if err == nil {
						_, err = s.Transfer(ctx, "agent-b", "agent-a", scope, false)
					}
					errs[i] = err
					return
				}
				_, errs[i] = s.Reserve(ctx, Lock{
					Type:       TypeFileReservation,
					Owner:      "agent-a",
					Scope:      scope,
					Pattern:    fmt.Sprintf("round%d/handle%d.go", r, i),
					Exclusive:  false,
					TTLSeconds: 60,
				})
			}(i, s)
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d handle %d: %v", r, i, err)
			}
		}
	}
}
