package dispatch

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"modernc.org/sqlite/vtab"
)

// A seam inside the driver for the lost-interrupt test. The driver arms its
// interrupt for a call, then prepares the statement, binds it and steps it.
// SQLite plans a query on a virtual table while it prepares, so the planner
// callback of a test-only virtual table runs after the interrupt is armed and
// before the statement is stepped, and it can hold the call there. No timing is
// involved: the callback signals that it is in that window and then waits, on
// the connection itself, until the driver has delivered the interrupt.

// seamState is what the test-only table talks to. It is swapped per test.
type seamState struct {
	entered   chan struct{} // closed when the planner callback is running
	planned   atomic.Bool   // set for the first planning only
	scanSteps atomic.Int32  // scan calls (Filter and Next) the statement made
	delivered func() bool   // reports that the interrupt reached the connection
	waitErr   atomic.Value  // string: why the wait for the interrupt gave up
}

var (
	seamMu     sync.Mutex
	seamActive *seamState
	seamOnce   sync.Once
)

func currentSeam() *seamState {
	seamMu.Lock()
	defer seamMu.Unlock()
	return seamActive
}

func setSeam(s *seamState) {
	seamMu.Lock()
	seamActive = s
	seamMu.Unlock()
}

type seamModule struct{}

func (seamModule) Create(ctx vtab.Context, _ []string) (vtab.Table, error) {
	if err := ctx.Declare("CREATE TABLE x(v INTEGER)"); err != nil {
		return nil, err
	}
	return seamTable{}, nil
}

func (m seamModule) Connect(ctx vtab.Context, a []string) (vtab.Table, error) {
	return m.Create(ctx, a)
}

type seamTable struct{}

func (seamTable) BestIndex(info *vtab.IndexInfo) error {
	info.EstimatedCost = 1
	info.EstimatedRows = 3
	s := currentSeam()
	if s == nil || !s.planned.CompareAndSwap(false, true) {
		return nil
	}
	close(s.entered)
	// Wait for the driver's own interrupt to land on the connection. Gosched is
	// a yield, not a delay: the loop ends when the condition holds, and the
	// interrupt goroutine is already armed, so it ends. A failure of that
	// assumption must not hang the suite, so the wait gives up after a bounded
	// number of yields and says so.
	for i := 0; !s.delivered(); i++ {
		if i > 50_000_000 {
			s.waitErr.Store("the interrupt never reached the connection")
			return nil
		}
		runtime.Gosched()
	}
	return nil
}

func (seamTable) Open() (vtab.Cursor, error) { return &seamCursor{}, nil }
func (seamTable) Disconnect() error          { return nil }
func (seamTable) Destroy() error             { return nil }

type seamCursor struct{ i int }

func (c *seamCursor) Filter(int, string, []vtab.Value) error { c.i = 0; return c.note() }
func (c *seamCursor) Next() error                            { c.i++; return c.note() }
func (c *seamCursor) Eof() bool                              { return c.i >= 3 }
func (c *seamCursor) Rowid() (int64, error)                  { return int64(c.i), nil }
func (c *seamCursor) Close() error                           { return nil }
func (c *seamCursor) Column(int) (vtab.Value, error)         { return int64(c.i), nil }

func (c *seamCursor) note() error {
	if s := currentSeam(); s != nil {
		s.scanSteps.Add(1)
	}
	return nil
}

func registerSeamModule(t *testing.T) {
	t.Helper()
	var err error
	seamOnce.Do(func() { err = vtab.RegisterModule(nil, "seamgate", seamModule{}) })
	if err != nil {
		t.Fatal(err)
	}
}
