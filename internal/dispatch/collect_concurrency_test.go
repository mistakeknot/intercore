package dispatch

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mistakeknot/intercore/internal/db"
)

// Several processes polling the same dispatch is the normal shape of
// `ic dispatch wait` from more than one coordinator. When the worker has
// already exited, every poller collects at once; the losers must observe the
// winner's terminal row instead of failing.
func TestPoll_ConcurrentCollectorsOfDeadDispatchAllSucceed(t *testing.T) {
	const (
		dispatches = 30
		collectors = 4
	)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")

	primary, err := db.Open(path, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { primary.Close() })
	if err := primary.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	seed := New(primary.SqlDB(), nil)

	ids := make([]string, dispatches)
	for i := range ids {
		id, err := seed.Create(ctx, &Dispatch{AgentType: "codex", ProjectDir: t.TempDir()})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := seed.UpdateStatus(ctx, id, StatusRunning, UpdateFields{
			"pid":        999999999,
			"started_at": time.Now().Unix(),
		}); err != nil {
			t.Fatalf("mark running: %v", err)
		}
		ids[i] = id
	}

	// One database handle per collector, as separate `ic` processes would have.
	stores := make([]*Store, collectors)
	for c := range stores {
		handle, err := db.Open(path, 100*time.Millisecond)
		if err != nil {
			t.Fatalf("Open collector %d: %v", c, err)
		}
		t.Cleanup(func() { handle.Close() })
		stores[c] = New(handle.SqlDB(), nil)
	}

	for _, id := range ids {
		start := make(chan struct{})
		errs := make([]error, collectors)
		statuses := make([]string, collectors)
		var wg sync.WaitGroup
		for c, store := range stores {
			wg.Add(1)
			go func(c int, store *Store) {
				defer wg.Done()
				<-start
				d, err := Poll(ctx, store, id)
				errs[c] = err
				if d != nil {
					statuses[c] = d.Status
				}
			}(c, store)
		}
		close(start)
		wg.Wait()

		for c, err := range errs {
			if err != nil {
				t.Fatalf("dispatch %s: collector %d failed while another collector terminalized it: %v", id, c, err)
			}
		}
		final, err := seed.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if !final.IsTerminal() {
			t.Fatalf("dispatch %s: status %q after concurrent collection, want terminal", id, final.Status)
		}
		for c, status := range statuses {
			if status != final.Status {
				t.Fatalf("dispatch %s: collector %d saw %q, recorded terminal status is %q", id, c, status, final.Status)
			}
		}
	}
}
