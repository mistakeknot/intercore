# Intercore Wave 2: Event Bus Implementation Plan
**Bead:** iv-egxf
**Phase:** executing (as of 2026-02-19T04:08:48Z)

> **For Claude:** REQUIRED SUB-SKILL: Use clavain:executing-plans to implement this plan task-by-task.

**Goal:** Add an event bus to intercore that makes phase transitions and dispatch changes reactive — in-process Notifier, polling cursor CLI, and three built-in handlers.

**Architecture:** New `internal/event/` package defines a unified `Event` type and `Notifier` with callback-based wiring (no cross-package interface). `Advance()` and dispatch `UpdateStatus()` call the Notifier after DB commits. A new `dispatch_events` table (schema v5) mirrors `phase_events` for dispatch lifecycle. The `ic events tail` CLI command reads from both tables via UNION query with **dual cursors** (separate per-table high-water marks) persisted in the `state` table. Three handlers (logging, auto-spawn, hook exec) register before advance. Hook handler runs asynchronously in a detached goroutine to avoid blocking the single DB connection.

**Deployment note:** Schema v5 migration requires no concurrent `ic` processes. Old binaries on a v5 DB will exit with `ErrSchemaVersionTooNew` — upgrade all binaries before running `ic init`.

**Tech Stack:** Go 1.22, `modernc.org/sqlite v1.29.0` (pure Go), existing intercore packages

---

### Task 1: Event Type + Event Store (F1 foundation)

**Files:**
- Create: `infra/intercore/internal/event/event.go`
- Create: `infra/intercore/internal/event/store.go`
- Create: `infra/intercore/internal/event/store_test.go`
- Modify: `infra/intercore/internal/db/schema.sql`
- Modify: `infra/intercore/internal/db/db.go:20-21` (bump schema version)

**Step 1: Write the Event type**

Create `infra/intercore/internal/event/event.go`:

```go
package event

import "time"

// Source identifies the origin subsystem.
const (
	SourcePhase    = "phase"
	SourceDispatch = "dispatch"
)

// Event is the unified event type for the intercore event bus.
type Event struct {
	ID        int64     `json:"id"`
	RunID     string    `json:"run_id"`
	Source    string    `json:"source"`     // "phase" or "dispatch"
	Type      string    `json:"type"`       // "advance", "skip", "block", "spawned", "completed", etc.
	FromState string    `json:"from_state"` // from_phase or from_status
	ToState   string    `json:"to_state"`   // to_phase or to_status
	Reason    string    `json:"reason,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}
```

**Step 2: Add dispatch_events table to schema**

Add to end of `infra/intercore/internal/db/schema.sql`:

```sql
-- v5: dispatch events (event bus)
CREATE TABLE IF NOT EXISTS dispatch_events (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    dispatch_id     TEXT NOT NULL,
    run_id          TEXT,
    from_status     TEXT NOT NULL,
    to_status       TEXT NOT NULL,
    event_type      TEXT NOT NULL DEFAULT 'status_change',
    reason          TEXT,
    created_at      INTEGER NOT NULL DEFAULT (unixepoch())
);
CREATE INDEX IF NOT EXISTS idx_dispatch_events_dispatch ON dispatch_events(dispatch_id);
CREATE INDEX IF NOT EXISTS idx_dispatch_events_run ON dispatch_events(run_id) WHERE run_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_dispatch_events_created ON dispatch_events(created_at);
```

Note: No FK on `dispatch_id` to `dispatches(id)` — dispatches may be pruned while events are retained. `run_id` is nullable because not all dispatches have a scope/run association.

**Step 3: Bump schema version in db.go**

In `infra/intercore/internal/db/db.go`, change:

```go
const (
	currentSchemaVersion = 5
	maxSchemaVersion     = 5
)
```

**Step 4: Write the EventStore**

Create `infra/intercore/internal/event/store.go`:

```go
package event

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Store provides event read/write operations.
type Store struct {
	db *sql.DB
}

// NewStore creates an event store.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// AddDispatchEvent records a dispatch lifecycle event.
func (s *Store) AddDispatchEvent(ctx context.Context, dispatchID, runID, fromStatus, toStatus, eventType, reason string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO dispatch_events (dispatch_id, run_id, from_status, to_status, event_type, reason)
		VALUES (?, NULLIF(?, ''), ?, ?, ?, NULLIF(?, ''))`,
		dispatchID, runID, fromStatus, toStatus, eventType, reason,
	)
	if err != nil {
		return fmt.Errorf("add dispatch event: %w", err)
	}
	return nil
}

// ListEvents returns unified events for a run, merging phase_events and
// dispatch_events, ordered by timestamp. Returns events with id > sinceID.
func (s *Store) ListEvents(ctx context.Context, runID string, sincePhaseID, sinceDispatchID int64, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 1000
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, run_id, 'phase' AS source, event_type, from_phase, to_phase,
			COALESCE(reason, '') AS reason, created_at
		FROM phase_events
		WHERE run_id = ? AND id > ?
		UNION ALL
		SELECT id, COALESCE(run_id, '') AS run_id, 'dispatch' AS source, event_type,
			from_status, to_status, COALESCE(reason, '') AS reason, created_at
		FROM dispatch_events
		WHERE (run_id = ? OR ? = '') AND id > ?
		ORDER BY created_at ASC, source ASC, id ASC
		LIMIT ?`,
		runID, sincePhaseID,
		runID, runID, sinceDispatchID,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		var e Event
		var createdAt int64
		if err := rows.Scan(&e.ID, &e.RunID, &e.Source, &e.Type,
			&e.FromState, &e.ToState, &e.Reason, &createdAt); err != nil {
			return nil, fmt.Errorf("list events scan: %w", err)
		}
		e.Timestamp = time.Unix(createdAt, 0)
		events = append(events, e)
	}
	return events, rows.Err()
}

// ListAllEvents returns events across all runs, merging both tables.
func (s *Store) ListAllEvents(ctx context.Context, sincePhaseID, sinceDispatchID int64, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 1000
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, run_id, 'phase' AS source, event_type, from_phase, to_phase,
			COALESCE(reason, '') AS reason, created_at
		FROM phase_events
		WHERE id > ?
		UNION ALL
		SELECT id, COALESCE(run_id, '') AS run_id, 'dispatch' AS source, event_type,
			from_status, to_status, COALESCE(reason, '') AS reason, created_at
		FROM dispatch_events
		WHERE id > ?
		ORDER BY created_at ASC, source ASC, id ASC
		LIMIT ?`,
		sincePhaseID, sinceDispatchID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list all events: %w", err)
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		var e Event
		var createdAt int64
		if err := rows.Scan(&e.ID, &e.RunID, &e.Source, &e.Type,
			&e.FromState, &e.ToState, &e.Reason, &createdAt); err != nil {
			return nil, fmt.Errorf("list all events scan: %w", err)
		}
		e.Timestamp = time.Unix(createdAt, 0)
		events = append(events, e)
	}
	return events, rows.Err()
}

// MaxPhaseEventID returns the highest phase_events.id (for cursor init).
func (s *Store) MaxPhaseEventID(ctx context.Context) (int64, error) {
	var id sql.NullInt64
	err := s.db.QueryRowContext(ctx, "SELECT MAX(id) FROM phase_events").Scan(&id)
	if err != nil {
		return 0, err
	}
	if !id.Valid {
		return 0, nil
	}
	return id.Int64, nil
}

// MaxDispatchEventID returns the highest dispatch_events.id (for cursor init).
func (s *Store) MaxDispatchEventID(ctx context.Context) (int64, error) {
	var id sql.NullInt64
	err := s.db.QueryRowContext(ctx, "SELECT MAX(id) FROM dispatch_events").Scan(&id)
	if err != nil {
		return 0, err
	}
	if !id.Valid {
		return 0, nil
	}
	return id.Int64, nil
}
```

**Step 5: Write tests for EventStore**

Create `infra/intercore/internal/event/store_test.go`:

```go
package event

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/mistakeknot/interverse/infra/intercore/internal/db"
)

func setupTestStore(t *testing.T) (*Store, *db.DB) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	d, err := db.Open(path, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	if err := d.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	return NewStore(d.SqlDB()), d
}

func TestAddDispatchEvent(t *testing.T) {
	store, _ := setupTestStore(t)
	ctx := context.Background()

	err := store.AddDispatchEvent(ctx, "disp001", "run001", "spawned", "running", "status_change", "")
	if err != nil {
		t.Fatalf("AddDispatchEvent: %v", err)
	}

	// Verify via MaxDispatchEventID
	maxID, err := store.MaxDispatchEventID(ctx)
	if err != nil {
		t.Fatalf("MaxDispatchEventID: %v", err)
	}
	if maxID < 1 {
		t.Errorf("MaxDispatchEventID = %d, want >= 1", maxID)
	}
}

func TestListEvents_MergesPhaseAndDispatch(t *testing.T) {
	store, d := setupTestStore(t)
	ctx := context.Background()

	// Insert a run so phase_events FK is satisfied
	_, err := d.SqlDB().ExecContext(ctx, `
		INSERT INTO runs (id, project_dir, goal, status, phase, complexity, force_full, auto_advance, created_at, updated_at)
		VALUES ('run001', '/tmp', 'test', 'active', 'brainstorm', 3, 0, 1, unixepoch(), unixepoch())`)
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}

	// Insert a phase event
	_, err = d.SqlDB().ExecContext(ctx, `
		INSERT INTO phase_events (run_id, from_phase, to_phase, event_type, reason)
		VALUES ('run001', 'brainstorm', 'strategized', 'advance', 'test')`)
	if err != nil {
		t.Fatalf("insert phase event: %v", err)
	}

	// Insert a dispatch event
	err = store.AddDispatchEvent(ctx, "disp001", "run001", "spawned", "running", "status_change", "started")
	if err != nil {
		t.Fatalf("AddDispatchEvent: %v", err)
	}

	events, err := store.ListEvents(ctx, "run001", 0, 0, 100)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}

	if len(events) != 2 {
		t.Fatalf("ListEvents count = %d, want 2", len(events))
	}

	// Both sources should be represented
	sources := map[string]bool{}
	for _, e := range events {
		sources[e.Source] = true
	}
	if !sources["phase"] {
		t.Error("missing phase event")
	}
	if !sources["dispatch"] {
		t.Error("missing dispatch event")
	}
}

func TestListEvents_SinceFiltering(t *testing.T) {
	store, d := setupTestStore(t)
	ctx := context.Background()

	// Insert run
	_, err := d.SqlDB().ExecContext(ctx, `
		INSERT INTO runs (id, project_dir, goal, status, phase, complexity, force_full, auto_advance, created_at, updated_at)
		VALUES ('run002', '/tmp', 'test', 'active', 'brainstorm', 3, 0, 1, unixepoch(), unixepoch())`)
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}

	// Insert 3 phase events
	for i := 0; i < 3; i++ {
		_, err = d.SqlDB().ExecContext(ctx, `
			INSERT INTO phase_events (run_id, from_phase, to_phase, event_type)
			VALUES ('run002', 'brainstorm', 'strategized', 'advance')`)
		if err != nil {
			t.Fatalf("insert phase event %d: %v", i, err)
		}
	}

	// Get all first
	all, err := store.ListEvents(ctx, "run002", 0, 0, 100)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("expected 3 events, got %d", len(all))
	}

	// Get since first event
	filtered, err := store.ListEvents(ctx, "run002", all[0].ID, 0, 100)
	if err != nil {
		t.Fatalf("ListEvents filtered: %v", err)
	}
	if len(filtered) != 2 {
		t.Errorf("expected 2 events after filtering, got %d", len(filtered))
	}
}
```

**Step 6: Run tests**

Run: `cd infra/intercore && go test ./internal/event/ -v`
Expected: ALL PASS (3 tests)

**Step 7: Run full test suite**

Run: `cd infra/intercore && go test ./... -v`
Expected: ALL PASS (existing tests pass with schema v5)

**Step 8: Commit**

```bash
cd infra/intercore && git add internal/event/event.go internal/event/store.go internal/event/store_test.go internal/db/schema.sql internal/db/db.go
git commit -m "feat(event): add unified Event type, dispatch_events table, EventStore (schema v5)"
```

---

### Task 2: In-Process Notifier Interface (F2)

**Files:**
- Create: `infra/intercore/internal/event/notifier.go`
- Create: `infra/intercore/internal/event/notifier_test.go`

**Step 1: Write the Notifier interface and implementation**

Create `infra/intercore/internal/event/notifier.go`:

```go
package event

import (
	"context"
	"fmt"
	"sync"
)

// Handler processes an event. Errors are logged but don't fail the parent operation.
type Handler func(ctx context.Context, e Event) error

// Notifier dispatches events to registered handlers.
type Notifier struct {
	mu       sync.RWMutex
	handlers []namedHandler
}

type namedHandler struct {
	name    string
	handler Handler
}

// NewNotifier creates a new event notifier.
func NewNotifier() *Notifier {
	return &Notifier{}
}

// Subscribe registers a named handler. Name is used for error logging.
func (n *Notifier) Subscribe(name string, h Handler) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.handlers = append(n.handlers, namedHandler{name: name, handler: h})
}

// Notify dispatches an event to all handlers synchronously.
// Handler errors are collected but do not stop dispatch to remaining handlers.
// Returns the first error encountered (if any) for logging purposes only.
func (n *Notifier) Notify(ctx context.Context, e Event) error {
	n.mu.RLock()
	handlers := make([]namedHandler, len(n.handlers))
	copy(handlers, n.handlers)
	n.mu.RUnlock()

	var firstErr error
	for _, nh := range handlers {
		if err := nh.handler(ctx, e); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("handler %s: %w", nh.name, err)
			}
		}
	}
	return firstErr
}

// HandlerCount returns the number of registered handlers.
func (n *Notifier) HandlerCount() int {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return len(n.handlers)
}
```

**Step 2: Write tests for Notifier**

Create `infra/intercore/internal/event/notifier_test.go`:

```go
package event

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestNotifier_Subscribe_And_Notify(t *testing.T) {
	n := NewNotifier()
	var called atomic.Int32

	n.Subscribe("test", func(ctx context.Context, e Event) error {
		called.Add(1)
		return nil
	})

	e := Event{
		Source:    SourcePhase,
		Type:      "advance",
		RunID:     "run001",
		FromState: "brainstorm",
		ToState:   "strategized",
		Timestamp: time.Now(),
	}

	err := n.Notify(context.Background(), e)
	if err != nil {
		t.Errorf("Notify returned error: %v", err)
	}
	if called.Load() != 1 {
		t.Errorf("handler called %d times, want 1", called.Load())
	}
}

func TestNotifier_MultipleHandlers(t *testing.T) {
	n := NewNotifier()
	var order []string

	n.Subscribe("first", func(ctx context.Context, e Event) error {
		order = append(order, "first")
		return nil
	})
	n.Subscribe("second", func(ctx context.Context, e Event) error {
		order = append(order, "second")
		return nil
	})

	n.Notify(context.Background(), Event{Source: SourcePhase, Type: "advance"})

	if len(order) != 2 || order[0] != "first" || order[1] != "second" {
		t.Errorf("handler order = %v, want [first, second]", order)
	}
}

func TestNotifier_HandlerError_DoesNotBlockOthers(t *testing.T) {
	n := NewNotifier()
	var secondCalled bool

	n.Subscribe("failing", func(ctx context.Context, e Event) error {
		return errors.New("boom")
	})
	n.Subscribe("succeeding", func(ctx context.Context, e Event) error {
		secondCalled = true
		return nil
	})

	err := n.Notify(context.Background(), Event{Source: SourcePhase, Type: "advance"})

	if err == nil {
		t.Error("expected error from failing handler")
	}
	if !secondCalled {
		t.Error("second handler was not called after first handler failed")
	}
}

func TestNotifier_NoHandlers(t *testing.T) {
	n := NewNotifier()

	err := n.Notify(context.Background(), Event{Source: SourcePhase, Type: "advance"})
	if err != nil {
		t.Errorf("Notify with no handlers returned error: %v", err)
	}
}
```

**Step 3: Run tests**

Run: `cd infra/intercore && go test ./internal/event/ -v`
Expected: ALL PASS

**Step 4: Commit**

```bash
cd infra/intercore && git add internal/event/notifier.go internal/event/notifier_test.go
git commit -m "feat(event): add in-process Notifier with subscribe/notify"
```

---

### Task 3: Wire Notifier into Advance() and Dispatch (F2 wiring)

**Files:**
- Modify: `infra/intercore/internal/phase/machine.go` (add Notifier parameter to Advance)
- Modify: `infra/intercore/internal/dispatch/dispatch.go` (add event recording to UpdateStatus)
- Modify: `infra/intercore/cmd/ic/main.go` (create Notifier at startup)
- Modify: `infra/intercore/cmd/ic/run.go:160-242` (pass Notifier to Advance)
- Modify: `infra/intercore/cmd/ic/dispatch.go` (pass event store to dispatch)
- Modify: `infra/intercore/internal/phase/machine_test.go` (update Advance calls)

**Step 1: Add PhaseEventCallback to Advance() signature**

In `infra/intercore/internal/phase/machine.go`, add a callback type and update the signature:

```go
// PhaseEventCallback is called after a successful phase transition.
// Errors are logged but do not fail the advance.
type PhaseEventCallback func(runID, eventType, fromPhase, toPhase, reason string)
```

Change `Advance()` signature to accept the callback (may be nil):

```go
func Advance(ctx context.Context, store *Store, runID string, cfg GateConfig, rt RuntrackQuerier, vq VerdictQuerier, callback PhaseEventCallback) (*AdvanceResult, error) {
```

After the successful phase update (after `AddEvent` succeeds), add:

```go
	if callback != nil {
		callback(runID, eventType, fromPhase, toPhase, reason)
	}
```

This uses a plain callback — no interface, no cross-package import from `phase` → `event`.

**Step 2: Update dispatch.Store with constructor-injected event recorder**

Change `dispatch.New()` to accept an optional recorder via constructor injection (no post-construction mutator):

```go
// DispatchEventRecorder is called after a dispatch status change.
// May be nil — UpdateStatus checks before calling.
type DispatchEventRecorder func(dispatchID, runID, fromStatus, toStatus string)

type Store struct {
	db            *sql.DB
	eventRecorder DispatchEventRecorder
}

// New creates a dispatch store. recorder may be nil if event recording is not needed.
func New(db *sql.DB, recorder DispatchEventRecorder) *Store {
	return &Store{db: db, eventRecorder: recorder}
}
```

In `UpdateStatus()`, capture `fromStatus` before the UPDATE and record the event inside the same transaction:

```go
func (s *Store) UpdateStatus(ctx context.Context, id string, fields UpdateFields) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Capture previous status BEFORE update (fixes P1: fromStatus always empty)
	var prevStatus string
	err = tx.QueryRowContext(ctx, "SELECT status FROM dispatches WHERE id = ?", id).Scan(&prevStatus)
	if err != nil {
		return fmt.Errorf("read previous status: %w", err)
	}

	// ... existing UPDATE logic using tx instead of s.db ...

	// Record event in same transaction (fixes P2: audit gap on crash)
	if s.eventRecorder != nil {
		newStatus, ok := fields["status"]
		if ok && newStatus != prevStatus {
			// Look up scopeID to find the run
			var scopeID sql.NullString
			tx.QueryRowContext(ctx, "SELECT scope_id FROM dispatches WHERE id = ?", id).Scan(&scopeID)
			runID := ""
			if scopeID.Valid {
				runID = scopeID.String
			}
			s.eventRecorder(id, runID, prevStatus, newStatus)
		}
	}

	return tx.Commit()
}
```

**Important:** All call sites of `dispatch.New()` must be updated to pass the recorder (or `nil` when not needed). This is a breaking change to the constructor signature.

**Step 3: Update CLI to create and wire the Notifier**

In `cmd/ic/run.go`, `cmdRunAdvance()`, create the Notifier and wire callbacks:

```go
	// Create event infrastructure
	evStore := event.NewStore(d.SqlDB())
	notifier := event.NewNotifier()
	// Register handlers here (Task 9 will add logging, spawn, hook handlers)

	// Phase event callback — bridges phase.Advance → event.Notifier
	var callback phase.PhaseEventCallback
	callback = func(runID, eventType, fromPhase, toPhase, reason string) {
		e := event.Event{
			RunID:     runID,
			Source:    event.SourcePhase,
			Type:      eventType,
			FromState: fromPhase,
			ToState:   toPhase,
			Reason:    reason,
			Timestamp: time.Now(),
		}
		notifier.Notify(ctx, e)
	}

	// Dispatch event recorder — bridges dispatch.UpdateStatus → event.Store + Notifier
	// Passed to dispatch.New() as constructor parameter (not post-construction mutator)
	recorder := func(dispatchID, runID, fromStatus, toStatus string) {
		evStore.AddDispatchEvent(ctx, dispatchID, runID, fromStatus, toStatus, "status_change", "")
		notifier.Notify(ctx, event.Event{
			RunID:     runID,
			Source:    event.SourceDispatch,
			Type:      "status_change",
			FromState: fromStatus,
			ToState:   toStatus,
			Timestamp: time.Now(),
		})
	}

	// Create dispatch store with event recorder via constructor injection
	dStore := dispatch.New(d.SqlDB(), recorder)

	result, err := phase.Advance(ctx, store, id, phase.GateConfig{...}, rtStore, dStore, callback)
```

**Important:** All existing `dispatch.New(db)` call sites must be updated to `dispatch.New(db, nil)` since the constructor signature now requires the recorder parameter.

**Step 4: Update all existing call sites**

Every call to `phase.Advance()` must pass the new callback parameter. For call sites that don't need notification (like `gate.go:EvaluateGate`), pass `nil`.

- `cmd/ic/run.go:cmdRunAdvance` → pass callback (done in Step 3)
- `cmd/ic/gate.go` (`EvaluateGate` doesn't call `Advance` directly, so no change needed)

Every call to `dispatch.New(db)` must be updated to `dispatch.New(db, nil)` (or with a recorder when event recording is desired):

- `cmd/ic/dispatch.go` → `dispatch.New(d.SqlDB(), nil)` (non-advance dispatch commands don't need recording)
- `cmd/ic/run.go:cmdRunAdvance` → `dispatch.New(d.SqlDB(), recorder)` (done in Step 3)
- `internal/phase/machine_test.go` → test dispatch stores pass `nil`

**Step 5: Update machine_test.go**

All `Advance()` calls in tests get `nil` as the last argument:

```go
result, err := Advance(ctx, store, id, GateConfig{Priority: 4}, nil, nil, nil)
```

**Step 6: Run tests**

Run: `cd infra/intercore && go test ./... -v`
Expected: ALL PASS

**Step 7: Commit**

```bash
cd infra/intercore && git add internal/phase/machine.go internal/dispatch/dispatch.go cmd/ic/run.go cmd/ic/main.go internal/phase/machine_test.go
git commit -m "feat(event): wire Notifier into Advance() and dispatch UpdateStatus"
```

---

### Task 4: Event Logging Handler (F3)

**Files:**
- Create: `infra/intercore/internal/event/handler_log.go`
- Create: `infra/intercore/internal/event/handler_log_test.go`

**Step 1: Write the logging handler**

Create `infra/intercore/internal/event/handler_log.go`:

```go
package event

import (
	"context"
	"fmt"
	"io"
	"os"
)

// NewLogHandler returns a handler that prints structured event lines.
// If quiet is true, logs are suppressed.
func NewLogHandler(w io.Writer, quiet bool) Handler {
	if w == nil {
		w = os.Stderr
	}
	return func(ctx context.Context, e Event) error {
		if quiet {
			return nil
		}
		fmt.Fprintf(w, "[event] source=%s type=%s run=%s from=%s to=%s",
			e.Source, e.Type, e.RunID, e.FromState, e.ToState)
		if e.Reason != "" {
			fmt.Fprintf(w, " reason=%q", e.Reason)
		}
		fmt.Fprintln(w)
		return nil
	}
}
```

**Step 2: Write tests**

Create `infra/intercore/internal/event/handler_log_test.go`:

```go
package event

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func TestLogHandler_OutputsStructuredLine(t *testing.T) {
	var buf bytes.Buffer
	h := NewLogHandler(&buf, false)

	e := Event{
		Source:    SourcePhase,
		Type:      "advance",
		RunID:     "abc12345",
		FromState: "brainstorm",
		ToState:   "strategized",
		Reason:    "Gate passed",
		Timestamp: time.Now(),
	}

	if err := h(context.Background(), e); err != nil {
		t.Fatal(err)
	}

	line := buf.String()
	if !strings.Contains(line, "[event]") {
		t.Errorf("missing [event] prefix: %s", line)
	}
	if !strings.Contains(line, "source=phase") {
		t.Errorf("missing source=phase: %s", line)
	}
	if !strings.Contains(line, "run=abc12345") {
		t.Errorf("missing run ID: %s", line)
	}
	if !strings.Contains(line, `reason="Gate passed"`) {
		t.Errorf("missing reason: %s", line)
	}
}

func TestLogHandler_Quiet(t *testing.T) {
	var buf bytes.Buffer
	h := NewLogHandler(&buf, true)

	h(context.Background(), Event{Source: SourcePhase, Type: "advance"})

	if buf.Len() != 0 {
		t.Errorf("quiet handler produced output: %s", buf.String())
	}
}

func TestLogHandler_NoReason(t *testing.T) {
	var buf bytes.Buffer
	h := NewLogHandler(&buf, false)

	h(context.Background(), Event{Source: SourceDispatch, Type: "status_change", RunID: "r1", FromState: "spawned", ToState: "running"})

	line := buf.String()
	if strings.Contains(line, "reason=") {
		t.Errorf("should not have reason field: %s", line)
	}
}
```

**Step 3: Run tests**

Run: `cd infra/intercore && go test ./internal/event/ -v -run TestLog`
Expected: ALL PASS

**Step 4: Commit**

```bash
cd infra/intercore && git add internal/event/handler_log.go internal/event/handler_log_test.go
git commit -m "feat(event): add structured event logging handler"
```

---

### Task 5: Auto-Agent-Spawn Handler (F4)

**Files:**
- Create: `infra/intercore/internal/event/handler_spawn.go`
- Create: `infra/intercore/internal/event/handler_spawn_test.go`
- Modify: `infra/intercore/internal/runtrack/store.go` (add `ListPendingAgentIDs` method)

**Step 0: Add ListPendingAgentIDs to runtrack.Store**

In `infra/intercore/internal/runtrack/store.go`, add:

```go
// ListPendingAgentIDs returns agent IDs with status="active" for a run.
func (s *Store) ListPendingAgentIDs(ctx context.Context, runID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM run_agents WHERE run_id = ? AND status = 'active'`, runID)
	if err != nil {
		return nil, fmt.Errorf("list pending agents: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
```

**Step 1: Write the spawn handler**

Create `infra/intercore/internal/event/handler_spawn.go`:

```go
package event

import (
	"context"
	"fmt"
	"io"
	"os"
)

// AgentQuerier queries run agents. Decouples from runtrack package.
type AgentQuerier interface {
	// ListPendingAgents returns agents with status="active" for a run.
	// (In runtrack, "active" means not yet completed.)
	ListPendingAgentIDs(ctx context.Context, runID string) ([]string, error)
}

// AgentSpawner triggers agent spawn by ID. Decouples from dispatch package.
type AgentSpawner interface {
	SpawnByAgentID(ctx context.Context, agentID string) error
}

// NewSpawnHandler returns a handler that auto-spawns agents when phase
// reaches "executing". No-op for other phase transitions or dispatch events.
func NewSpawnHandler(querier AgentQuerier, spawner AgentSpawner, logw io.Writer) Handler {
	if logw == nil {
		logw = os.Stderr
	}
	return func(ctx context.Context, e Event) error {
		// Only trigger on phase advance to executing
		if e.Source != SourcePhase || e.ToState != "executing" {
			return nil
		}

		agentIDs, err := querier.ListPendingAgentIDs(ctx, e.RunID)
		if err != nil {
			return fmt.Errorf("auto-spawn: list agents: %w", err)
		}

		if len(agentIDs) == 0 {
			return nil
		}

		for _, id := range agentIDs {
			if err := spawner.SpawnByAgentID(ctx, id); err != nil {
				fmt.Fprintf(logw, "[event] auto-spawn: agent %s failed: %v\n", id, err)
				continue
			}
			fmt.Fprintf(logw, "[event] auto-spawn: agent %s started\n", id)
		}
		return nil
	}
}
```

**Step 2: Write tests with mock interfaces**

Create `infra/intercore/internal/event/handler_spawn_test.go`:

```go
package event

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

type mockQuerier struct {
	agents []string
	err    error
}

func (m *mockQuerier) ListPendingAgentIDs(ctx context.Context, runID string) ([]string, error) {
	return m.agents, m.err
}

type mockSpawner struct {
	spawned []string
	failIDs map[string]bool
}

func (m *mockSpawner) SpawnByAgentID(ctx context.Context, agentID string) error {
	if m.failIDs[agentID] {
		return errors.New("spawn failed")
	}
	m.spawned = append(m.spawned, agentID)
	return nil
}

func TestSpawnHandler_TriggersOnExecuting(t *testing.T) {
	q := &mockQuerier{agents: []string{"agent1", "agent2"}}
	s := &mockSpawner{failIDs: map[string]bool{}}
	var buf bytes.Buffer
	h := NewSpawnHandler(q, s, &buf)

	e := Event{
		Source:    SourcePhase,
		Type:      "advance",
		RunID:     "run001",
		FromState: "planned",
		ToState:   "executing",
		Timestamp: time.Now(),
	}

	err := h(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}

	if len(s.spawned) != 2 {
		t.Errorf("spawned %d agents, want 2", len(s.spawned))
	}
}

func TestSpawnHandler_IgnoresNonExecuting(t *testing.T) {
	q := &mockQuerier{agents: []string{"agent1"}}
	s := &mockSpawner{failIDs: map[string]bool{}}
	h := NewSpawnHandler(q, s, nil)

	// Advance to strategized — should not trigger spawn
	e := Event{Source: SourcePhase, Type: "advance", ToState: "strategized"}
	h(context.Background(), e)

	if len(s.spawned) != 0 {
		t.Errorf("should not spawn for non-executing phase, spawned %d", len(s.spawned))
	}
}

func TestSpawnHandler_IgnoresDispatchEvents(t *testing.T) {
	q := &mockQuerier{agents: []string{"agent1"}}
	s := &mockSpawner{failIDs: map[string]bool{}}
	h := NewSpawnHandler(q, s, nil)

	e := Event{Source: SourceDispatch, Type: "status_change", ToState: "executing"}
	h(context.Background(), e)

	if len(s.spawned) != 0 {
		t.Errorf("should not spawn for dispatch events")
	}
}

func TestSpawnHandler_NoAgents(t *testing.T) {
	q := &mockQuerier{agents: nil}
	s := &mockSpawner{failIDs: map[string]bool{}}
	h := NewSpawnHandler(q, s, nil)

	e := Event{Source: SourcePhase, Type: "advance", RunID: "run001", ToState: "executing"}
	err := h(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.spawned) != 0 {
		t.Errorf("should not spawn with no agents")
	}
}

func TestSpawnHandler_PartialFailure(t *testing.T) {
	q := &mockQuerier{agents: []string{"ok1", "fail1", "ok2"}}
	s := &mockSpawner{failIDs: map[string]bool{"fail1": true}}
	var buf bytes.Buffer
	h := NewSpawnHandler(q, s, &buf)

	e := Event{Source: SourcePhase, Type: "advance", RunID: "run001", ToState: "executing"}
	h(context.Background(), e)

	if len(s.spawned) != 2 {
		t.Errorf("spawned %d, want 2 (ok1 and ok2)", len(s.spawned))
	}
	if !bytes.Contains(buf.Bytes(), []byte("fail1 failed")) {
		t.Error("expected failure log for fail1")
	}
}
```

**Step 3: Run tests**

Run: `cd infra/intercore && go test ./internal/event/ -v -run TestSpawn`
Expected: ALL PASS

**Step 4: Commit**

```bash
cd infra/intercore && git add internal/event/handler_spawn.go internal/event/handler_spawn_test.go
git commit -m "feat(event): add auto-agent-spawn handler for executing phase"
```

---

### Task 6: Shell Hook Trigger Handler (F5)

**Files:**
- Create: `infra/intercore/internal/event/handler_hook.go`
- Create: `infra/intercore/internal/event/handler_hook_test.go`

**Step 1: Write the hook handler**

Create `infra/intercore/internal/event/handler_hook.go`:

```go
package event

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

const (
	hookPhaseAdvance   = "on-phase-advance"
	hookDispatchChange = "on-dispatch-change"
	hookTimeout        = 5 * time.Second
)

// NewHookHandler returns a handler that executes convention-based shell hooks.
// projectDir is the base directory where .clavain/hooks/ is searched.
// Hooks run in a detached goroutine to avoid blocking the single DB connection.
// The handler returns immediately — hook results are logged asynchronously.
func NewHookHandler(projectDir string, logw io.Writer) Handler {
	if logw == nil {
		logw = os.Stderr
	}
	return func(ctx context.Context, e Event) error {
		var hookName string
		switch e.Source {
		case SourcePhase:
			hookName = hookPhaseAdvance
		case SourceDispatch:
			hookName = hookDispatchChange
		default:
			return nil
		}

		hookPath := filepath.Join(projectDir, ".clavain", "hooks", hookName)
		info, err := os.Stat(hookPath)
		if err != nil || info.IsDir() {
			return nil // hook doesn't exist, silently skip
		}
		if info.Mode()&0111 == 0 {
			return nil // not executable
		}

		// Serialize event as JSON for stdin (use json.Marshal on Event directly
		// to preserve all fields and future additions)
		eventJSON, err := json.Marshal(e)
		if err != nil {
			return fmt.Errorf("hook: marshal event: %w", err)
		}

		// Run hook in detached goroutine with independent context and timeout.
		// This avoids blocking the Notifier call chain and the parent's single
		// DB connection (ic uses SetMaxOpenConns(1)). A synchronous hook that
		// calls back into `ic` (e.g., `ic run status`) would deadlock otherwise.
		go func() {
			hookCtx, cancel := context.WithTimeout(context.Background(), hookTimeout)
			defer cancel()

			cmd := exec.CommandContext(hookCtx, hookPath)
			cmd.Stdin = bytes.NewReader(eventJSON)
			cmd.Dir = projectDir

			var stderr bytes.Buffer
			cmd.Stderr = &stderr

			if err := cmd.Run(); err != nil {
				fmt.Fprintf(logw, "[event] hook %s failed: %v (stderr: %s)\n",
					hookName, err, stderr.String())
			}
		}()

		return nil
	}
}
```

**Step 2: Write tests**

Create `infra/intercore/internal/event/handler_hook_test.go`:

```go
package event

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHookHandler_ExecutesPhaseHook(t *testing.T) {
	dir := t.TempDir()
	hookDir := filepath.Join(dir, ".clavain", "hooks")
	os.MkdirAll(hookDir, 0755)

	// Write a hook that writes event JSON to a file
	outputFile := filepath.Join(dir, "hook-output.json")
	hookScript := "#!/bin/sh\ncat > " + outputFile + "\n"
	hookPath := filepath.Join(hookDir, "on-phase-advance")
	os.WriteFile(hookPath, []byte(hookScript), 0755)

	var buf bytes.Buffer
	h := NewHookHandler(dir, &buf)

	e := Event{
		Source:    SourcePhase,
		Type:      "advance",
		RunID:     "run001",
		FromState: "brainstorm",
		ToState:   "strategized",
		Timestamp: time.Now(),
	}

	err := h(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}

	// Hook runs in a goroutine — wait briefly for it to complete
	time.Sleep(200 * time.Millisecond)

	// Verify hook was called — output file should exist
	data, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("hook output not written: %v", err)
	}
	if len(data) == 0 {
		t.Error("hook output is empty")
	}
}

func TestHookHandler_SkipsIfNoHook(t *testing.T) {
	dir := t.TempDir() // no .clavain/hooks/ directory
	var buf bytes.Buffer
	h := NewHookHandler(dir, &buf)

	err := h(context.Background(), Event{Source: SourcePhase, Type: "advance"})
	if err != nil {
		t.Fatal(err)
	}
	// No error, no panic — hook simply skipped
}

func TestHookHandler_SkipsNonExecutable(t *testing.T) {
	dir := t.TempDir()
	hookDir := filepath.Join(dir, ".clavain", "hooks")
	os.MkdirAll(hookDir, 0755)

	// Write hook without execute permission
	hookPath := filepath.Join(hookDir, "on-phase-advance")
	os.WriteFile(hookPath, []byte("#!/bin/sh\necho test"), 0644) // no +x

	var buf bytes.Buffer
	h := NewHookHandler(dir, &buf)

	err := h(context.Background(), Event{Source: SourcePhase, Type: "advance"})
	if err != nil {
		t.Fatal(err)
	}
	// Should skip silently — no output, no error
}

func TestHookHandler_FailingHookIsFireAndForget(t *testing.T) {
	dir := t.TempDir()
	hookDir := filepath.Join(dir, ".clavain", "hooks")
	os.MkdirAll(hookDir, 0755)

	hookPath := filepath.Join(hookDir, "on-phase-advance")
	os.WriteFile(hookPath, []byte("#!/bin/sh\nexit 1"), 0755)

	var buf bytes.Buffer
	h := NewHookHandler(dir, &buf)

	err := h(context.Background(), Event{Source: SourcePhase, Type: "advance"})
	if err != nil {
		t.Fatalf("hook failure should not return error, got: %v", err)
	}

	// Hook runs in a goroutine — wait for failure log
	time.Sleep(200 * time.Millisecond)

	if !bytes.Contains(buf.Bytes(), []byte("failed")) {
		t.Error("expected failure log")
	}
}

func TestHookHandler_DispatchEvent(t *testing.T) {
	dir := t.TempDir()
	hookDir := filepath.Join(dir, ".clavain", "hooks")
	os.MkdirAll(hookDir, 0755)

	outputFile := filepath.Join(dir, "dispatch-output.json")
	hookScript := "#!/bin/sh\ncat > " + outputFile + "\n"
	hookPath := filepath.Join(hookDir, "on-dispatch-change")
	os.WriteFile(hookPath, []byte(hookScript), 0755)

	h := NewHookHandler(dir, nil)
	e := Event{Source: SourceDispatch, Type: "status_change", RunID: "run001"}
	h(context.Background(), e)

	// Hook runs in a goroutine — wait for completion
	time.Sleep(200 * time.Millisecond)

	if _, err := os.Stat(outputFile); err != nil {
		t.Fatalf("dispatch hook not executed: %v", err)
	}
}
```

**Step 3: Run tests**

Run: `cd infra/intercore && go test ./internal/event/ -v -run TestHook`
Expected: ALL PASS

**Step 4: Commit**

```bash
cd infra/intercore && git add internal/event/handler_hook.go internal/event/handler_hook_test.go
git commit -m "feat(event): add convention-based shell hook handler"
```

---

### Task 7: `ic events tail` CLI Command (F6)

**Files:**
- Create: `infra/intercore/cmd/ic/events.go`
- Modify: `infra/intercore/cmd/ic/main.go:75-100` (add "events" case to switch)

**Step 1: Write the events CLI command**

Create `infra/intercore/cmd/ic/events.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mistakeknot/interverse/infra/intercore/internal/event"
	"github.com/mistakeknot/interverse/infra/intercore/internal/state"
)

func cmdEvents(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "ic: events: missing subcommand (tail, cursor)\n")
		return 3
	}

	switch args[0] {
	case "tail":
		return cmdEventsTail(ctx, args[1:])
	case "cursor":
		return cmdEventsCursor(ctx, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "ic: events: unknown subcommand: %s\n", args[0])
		return 3
	}
}

func cmdEventsTail(ctx context.Context, args []string) int {
	var runID, consumer string
	var follow bool
	var sincePhase, sinceDispatch int64
	var allRuns bool
	pollInterval := 500 * time.Millisecond
	limit := 100

	var positional []string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--follow" || args[i] == "-f":
			follow = true
		case args[i] == "--all":
			allRuns = true
		case strings.HasPrefix(args[i], "--since-phase="):
			val := strings.TrimPrefix(args[i], "--since-phase=")
			n, err := strconv.ParseInt(val, 10, 64)
			if err != nil {
				fmt.Fprintf(os.Stderr, "ic: events tail: invalid --since-phase: %s\n", val)
				return 3
			}
			sincePhase = n
		case strings.HasPrefix(args[i], "--since-dispatch="):
			val := strings.TrimPrefix(args[i], "--since-dispatch=")
			n, err := strconv.ParseInt(val, 10, 64)
			if err != nil {
				fmt.Fprintf(os.Stderr, "ic: events tail: invalid --since-dispatch: %s\n", val)
				return 3
			}
			sinceDispatch = n
		case strings.HasPrefix(args[i], "--consumer="):
			consumer = strings.TrimPrefix(args[i], "--consumer=")
		case strings.HasPrefix(args[i], "--poll-interval="):
			val := strings.TrimPrefix(args[i], "--poll-interval=")
			d, err := time.ParseDuration(val)
			if err != nil {
				fmt.Fprintf(os.Stderr, "ic: events tail: invalid --poll-interval: %s\n", val)
				return 3
			}
			pollInterval = d
		case strings.HasPrefix(args[i], "--limit="):
			val := strings.TrimPrefix(args[i], "--limit=")
			n, err := strconv.Atoi(val)
			if err != nil {
				fmt.Fprintf(os.Stderr, "ic: events tail: invalid --limit: %s\n", val)
				return 3
			}
			limit = n
		default:
			positional = append(positional, args[i])
		}
	}

	if len(positional) > 0 {
		runID = positional[0]
	}

	if runID == "" && !allRuns {
		fmt.Fprintf(os.Stderr, "ic: events tail: provide <run_id> or --all\n")
		return 3
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: events tail: %v\n", err)
		return 2
	}
	defer d.Close()

	evStore := event.NewStore(d.SqlDB())
	stStore := state.New(d.SqlDB())

	// Restore cursor if consumer is named
	if consumer != "" && sincePhase == 0 && sinceDispatch == 0 {
		sincePhase, sinceDispatch = loadCursor(ctx, stStore, consumer, runID)
	}

	enc := json.NewEncoder(os.Stdout)

	for {
		var events []event.Event
		var err error

		if allRuns || runID == "" {
			events, err = evStore.ListAllEvents(ctx, sincePhase, sinceDispatch, limit)
		} else {
			events, err = evStore.ListEvents(ctx, runID, sincePhase, sinceDispatch, limit)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "ic: events tail: %v\n", err)
			return 2
		}

		for _, e := range events {
			enc.Encode(e)
			// Track high water mark per source
			if e.Source == event.SourcePhase && e.ID > sincePhase {
				sincePhase = e.ID
			}
			if e.Source == event.SourceDispatch && e.ID > sinceDispatch {
				sinceDispatch = e.ID
			}
		}

		// Save cursor after each batch
		if consumer != "" && len(events) > 0 {
			saveCursor(ctx, stStore, consumer, runID, sincePhase, sinceDispatch)
		}

		if !follow {
			break
		}

		time.Sleep(pollInterval)
	}

	return 0
}

func cmdEventsCursor(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "ic: events cursor: missing subcommand (list, reset)\n")
		return 3
	}

	switch args[0] {
	case "list":
		return cmdEventsCursorList(ctx)
	case "reset":
		return cmdEventsCursorReset(ctx, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "ic: events cursor: unknown subcommand: %s\n", args[0])
		return 3
	}
}

func cmdEventsCursorList(ctx context.Context) int {
	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: events cursor list: %v\n", err)
		return 2
	}
	defer d.Close()

	stStore := state.New(d.SqlDB())
	ids, err := stStore.List(ctx, "cursor")
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: events cursor list: %v\n", err)
		return 2
	}

	for _, id := range ids {
		payload, err := stStore.Get(ctx, "cursor", id)
		if err != nil {
			continue
		}
		fmt.Printf("%s\t%s\n", id, string(payload))
	}
	return 0
}

func cmdEventsCursorReset(ctx context.Context, args []string) int {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "ic: events cursor reset: usage: ic events cursor reset <consumer-name>\n")
		return 3
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: events cursor reset: %v\n", err)
		return 2
	}
	defer d.Close()

	stStore := state.New(d.SqlDB())
	deleted, err := stStore.Delete(ctx, "cursor", args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: events cursor reset: %v\n", err)
		return 2
	}

	if deleted {
		fmt.Println("reset")
	} else {
		fmt.Println("not found")
	}
	return 0
}

// --- cursor helpers ---

func loadCursor(ctx context.Context, store *state.Store, consumer, scope string) (int64, int64) {
	key := consumer
	if scope != "" {
		key = consumer + ":" + scope
	}
	payload, err := store.Get(ctx, "cursor", key)
	if err != nil {
		return 0, 0
	}

	var cursor struct {
		Phase    int64 `json:"phase"`
		Dispatch int64 `json:"dispatch"`
	}
	if err := json.Unmarshal(payload, &cursor); err != nil {
		return 0, 0
	}
	return cursor.Phase, cursor.Dispatch
}

func saveCursor(ctx context.Context, store *state.Store, consumer, scope string, phaseID, dispatchID int64) {
	key := consumer
	if scope != "" {
		key = consumer + ":" + scope
	}
	payload := fmt.Sprintf(`{"phase":%d,"dispatch":%d}`, phaseID, dispatchID)
	// 24h TTL for auto-cleanup of abandoned cursors
	if err := store.Set(ctx, "cursor", key, json.RawMessage(payload), 24*time.Hour); err != nil {
		fmt.Fprintf(os.Stderr, "[event] saveCursor %s: %v\n", key, err)
	}
}
```

**Step 2: Add "events" to main.go command router**

In `infra/intercore/cmd/ic/main.go`, add to the switch statement (around line 89, before `case "gate":`):

```go
	case "events":
		exitCode = cmdEvents(ctx, subArgs)
```

Also add `"events"` to the `printUsage()` help text.

**Step 3: Run build**

Run: `cd infra/intercore && go build -o ic ./cmd/ic`
Expected: Compiles cleanly

**Step 4: Run integration test manually**

Run:
```bash
cd infra/intercore
./ic init
./ic run create --project=. --goal="Test events"
# Note the run ID output
./ic run advance <run_id>
./ic events tail <run_id>
./ic events tail <run_id> --consumer=test --since-phase=0 --since-dispatch=0
./ic events cursor list
```
Expected: Events printed as JSON lines with dual cursors, cursor persisted

**Step 5: Commit**

```bash
cd infra/intercore && git add cmd/ic/events.go cmd/ic/main.go
git commit -m "feat(event): add ic events tail CLI with cursor support"
```

---

### Task 8: Bash Library Wrappers (F7)

**Files:**
- Modify: `infra/intercore/lib-intercore.sh` (add event wrappers, bump version)

**Step 1: Add event wrappers to lib-intercore.sh**

Append these functions before the final comment block. Bump version to `0.6.0`:

```bash
# --- Event bus wrappers (v0.6.0) ---

# intercore_events_tail <run_id> [--since=N]
# One-shot event dump. Returns JSON lines to stdout.
intercore_events_tail() {
    intercore_available || return 0
    local run_id="$1"; shift
    $INTERCORE_BIN events tail "$run_id" "$@" 2>/dev/null
}

# intercore_events_tail_all [--since=N]
# One-shot event dump across all runs.
intercore_events_tail_all() {
    intercore_available || return 0
    $INTERCORE_BIN events tail --all "$@" 2>/dev/null
}

# intercore_events_cursor_get <consumer>
# Returns cursor JSON payload or empty string.
# Uses `ic events cursor list` rather than reading state directly,
# so cursor format changes don't silently break the wrapper.
intercore_events_cursor_get() {
    intercore_available || { echo ""; return 0; }
    local consumer="$1"
    $INTERCORE_BIN events cursor list 2>/dev/null | grep "^${consumer}	" | cut -f2 || echo ""
}

# intercore_events_cursor_set <consumer> <phase_id> <dispatch_id>
# Manually set cursor position.
intercore_events_cursor_set() {
    intercore_available || return 0
    local consumer="$1" phase_id="$2" dispatch_id="$3"
    echo "{\"phase\":${phase_id},\"dispatch\":${dispatch_id}}" | \
        $INTERCORE_BIN state set "cursor" "$consumer" --ttl=24h 2>/dev/null
}

# intercore_events_cursor_reset <consumer>
# Reset a consumer's cursor.
intercore_events_cursor_reset() {
    intercore_available || return 0
    $INTERCORE_BIN events cursor reset "$1" 2>/dev/null
}
```

Also change the version constant at the top:

```bash
INTERCORE_WRAPPER_VERSION="0.6.0"
```

**Step 2: Run existing integration tests**

Run: `cd infra/intercore && bash test-integration.sh`
Expected: ALL PASS (existing tests + new binary features)

**Step 3: Commit**

```bash
cd infra/intercore && git add lib-intercore.sh
git commit -m "feat(event): add bash wrappers for events tail and cursor (v0.6.0)"
```

---

### Task 9: Register Handlers in cmdRunAdvance Before Calling Advance

**Files:**
- Modify: `infra/intercore/cmd/ic/run.go` (register handlers in cmdRunAdvance)
- Modify: `infra/intercore/cmd/ic/main.go` (version bump to 0.3.0)

**Step 1: Wire handlers into cmdRunAdvance**

In `cmdRunAdvance()`, after opening the DB and creating stores, create the Notifier and register all three handlers:

```go
	evStore := event.NewStore(d.SqlDB())
	notifier := event.NewNotifier()

	// Register handlers
	notifier.Subscribe("log", event.NewLogHandler(os.Stderr, !flagVerbose))
	// Auto-spawn and hook handlers need project dir from the run
	// We'll register them after getting the run info
```

For the spawn handler, since it needs interface implementations from `runtrack` and `dispatch`, create adapter functions in the CLI code or pass them as closures.

**Step 2: Bump ic version**

In `cmd/ic/main.go`:

```go
const version = "0.3.0"
```

**Step 3: Build and test**

Run:
```bash
cd infra/intercore && go build -o ic ./cmd/ic
./ic version
go test ./... -v
```
Expected: `ic 0.3.0`, schema v5, ALL PASS

**Step 4: Run full integration test**

Run: `cd infra/intercore && bash test-integration.sh`
Expected: ALL PASS

**Step 5: Commit**

```bash
cd infra/intercore && git add cmd/ic/run.go cmd/ic/main.go
git commit -m "feat(event): register all handlers at startup, bump ic to 0.3.0"
```

---

### Task 10: Final Integration — End-to-End Test

**Files:**
- Modify: `infra/intercore/test-integration.sh` (add event bus test cases)

**Step 1: Add event bus integration tests**

Append to `test-integration.sh`:

```bash
# --- Event Bus Tests ---

echo "=== Event bus tests ==="

# Create a run and advance it
RUN_ID=$($IC run create --project="$TMPDIR" --goal="Event bus test" 2>&1)
assert_ok "create run for events"

$IC run advance "$RUN_ID" 2>&1
assert_ok "advance run"

# Tail events — should have at least one event
EVENTS=$($IC events tail "$RUN_ID" --json 2>&1)
assert_ok "events tail"
echo "$EVENTS" | grep -q "phase" || fail "events tail: no phase events"

# Test cursor
$IC events tail "$RUN_ID" --consumer=test-consumer 2>&1
assert_ok "events tail with consumer"

$IC events cursor list 2>&1 | grep -q "test-consumer" || fail "cursor not persisted"

$IC events cursor reset test-consumer 2>&1
assert_ok "cursor reset"

echo "Event bus tests passed"
```

**Step 2: Run integration tests**

Run: `cd infra/intercore && bash test-integration.sh`
Expected: ALL PASS including new event bus tests

**Step 3: Final commit**

```bash
cd infra/intercore && git add test-integration.sh
git commit -m "test(event): add event bus integration tests"
```
