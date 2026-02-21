package portfolio

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/mistakeknot/interverse/infra/intercore/internal/phase"
	"github.com/mistakeknot/interverse/infra/intercore/internal/state"
)

// Event type constants for portfolio relay events.
const (
	EventChildAdvanced   = "child_advanced"
	EventChildCompleted  = "child_completed"
	EventChildCancelled  = "child_cancelled"
	EventChildBlocked    = "child_blocked"
	EventChildRolledBack = "child_rolledback"
	EventUpstreamChanged = "upstream_changed"
)

// Relay polls child project databases and relays events to the portfolio run.
type Relay struct {
	portfolioID string
	db          *sql.DB
	store       *phase.Store
	stateStore  *state.Store
	depStore    *DepStore
	pool        *DBPool
	interval    time.Duration
	logw        io.Writer
}

// NewRelay creates a portfolio event relay.
func NewRelay(portfolioID string, db *sql.DB, interval time.Duration) *Relay {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	return &Relay{
		portfolioID: portfolioID,
		db:          db,
		store:       phase.New(db),
		stateStore:  state.New(db),
		depStore:    NewDepStore(db),
		pool:        NewDBPool(500 * time.Millisecond),
		interval:    interval,
		logw:        os.Stderr,
	}
}

// SetLogWriter sets the writer for relay log output.
func (r *Relay) SetLogWriter(w io.Writer) {
	r.logw = w
}

// Run starts the relay loop. Blocks until ctx is cancelled.
func (r *Relay) Run(ctx context.Context) error {
	defer r.pool.Close()

	// Load cursors from state table
	cursors, err := r.loadCursors(ctx)
	if err != nil {
		return fmt.Errorf("relay: load cursors: %w", err)
	}

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		if err := r.poll(ctx, cursors); err != nil {
			fmt.Fprintf(r.logw, "[relay] poll error: %v\n", err)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// mapChildEventType maps a child phase event type to a portfolio relay event type.
func mapChildEventType(childEventType string) string {
	switch childEventType {
	case phase.EventCancel:
		return EventChildCancelled
	case phase.EventBlock, phase.EventPause:
		return EventChildBlocked
	case phase.EventRollback:
		return EventChildRolledBack
	default:
		return EventChildAdvanced
	}
}

// poll executes a single relay cycle.
func (r *Relay) poll(ctx context.Context, cursors map[string]int64) error {
	// Load portfolio and children
	children, err := r.store.GetChildren(ctx, r.portfolioID)
	if err != nil {
		return fmt.Errorf("poll: get children: %w", err)
	}

	// Load dependency edges
	deps, err := r.depStore.List(ctx, r.portfolioID)
	if err != nil {
		return fmt.Errorf("poll: list deps: %w", err)
	}

	// Count active dispatches across all children
	totalActive := 0

	for _, child := range children {
		if phase.IsTerminalStatus(child.Status) {
			continue
		}

		childDB, err := r.pool.Get(child.ProjectDir)
		if err != nil {
			fmt.Fprintf(r.logw, "[relay] skip %s: %v\n", child.ProjectDir, err)
			continue
		}

		cursor := cursors[child.ProjectDir]

		// Query new phase events from child DB
		events, err := queryChildEvents(ctx, childDB, cursor)
		if err != nil {
			fmt.Fprintf(r.logw, "[relay] query events %s: %v\n", child.ProjectDir, err)
			continue
		}

		// Process events and persist atomically per child
		if len(events) > 0 {
			newCursor, err := r.relayChildEvents(ctx, child.ProjectDir, cursor, events, deps)
			if err != nil {
				fmt.Fprintf(r.logw, "[relay] relay events %s: %v\n", child.ProjectDir, err)
				continue
			}
			cursors[child.ProjectDir] = newCursor
		}

		// Count active dispatches in child project
		active, err := countActiveDispatches(ctx, childDB)
		if err == nil {
			totalActive += active
		}
	}

	// Write active dispatch count to state table
	if err := r.stateStore.Set(ctx, "active-dispatch-count", r.portfolioID,
		json.RawMessage(strconv.Quote(strconv.Itoa(totalActive))), 0); err != nil {
		fmt.Fprintf(r.logw, "[relay] write dispatch count: %v\n", err)
	}

	return nil
}

// relayChildEvents atomically inserts relay events and advances the cursor
// for a single child within one transaction on the portfolio DB.
func (r *Relay) relayChildEvents(ctx context.Context, projectDir string, cursor int64, events []childEvent, deps []Dep) (int64, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return cursor, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	newCursor := cursor
	for _, evt := range events {
		relayReason := fmt.Sprintf("relay:%s", projectDir)
		eventType := mapChildEventType(evt.EventType)

		_, err := tx.ExecContext(ctx, `
			INSERT INTO phase_events (
				run_id, from_phase, to_phase, event_type, reason
			) VALUES (?, ?, ?, ?, ?)`,
			r.portfolioID, evt.FromPhase, evt.ToPhase, eventType, relayReason,
		)
		if err != nil {
			return cursor, fmt.Errorf("insert relay event: %w", err)
		}

		fmt.Fprintf(r.logw, "[relay] %s: %s %s→%s\n", projectDir, eventType, evt.FromPhase, evt.ToPhase)

		if evt.ID > newCursor {
			newCursor = evt.ID
		}

		// Check dependencies: did this child advance past an upstream boundary?
		for _, dep := range deps {
			if dep.UpstreamProject == projectDir {
				reason := fmt.Sprintf("%s:%s→%s", EventUpstreamChanged, evt.FromPhase, evt.ToPhase)
				_, err := tx.ExecContext(ctx, `
					INSERT INTO phase_events (
						run_id, from_phase, to_phase, event_type, reason
					) VALUES (?, ?, ?, ?, ?)`,
					r.portfolioID, evt.FromPhase, evt.ToPhase, EventUpstreamChanged, reason,
				)
				if err != nil {
					return cursor, fmt.Errorf("insert upstream event: %w", err)
				}
			}
		}
	}

	// Save cursor in the same transaction
	cursorVal := strconv.FormatInt(newCursor, 10)
	_, err = tx.ExecContext(ctx, `
		INSERT OR REPLACE INTO state (key, scope_id, payload, updated_at, expires_at)
		VALUES (?, ?, ?, ?, ?)`,
		"relay-cursor", projectDir,
		json.RawMessage(strconv.Quote(cursorVal)),
		time.Now().Unix(), 0,
	)
	if err != nil {
		return cursor, fmt.Errorf("save cursor: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return cursor, fmt.Errorf("commit: %w", err)
	}
	return newCursor, nil
}

// childEvent is a minimal phase event from a child DB.
type childEvent struct {
	ID        int64
	FromPhase string
	ToPhase   string
	EventType string
}

// queryChildEvents queries phase_events from a child DB since the given cursor.
func queryChildEvents(ctx context.Context, db *sql.DB, sinceID int64) ([]childEvent, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, from_phase, to_phase, event_type
		FROM phase_events WHERE id > ? ORDER BY id ASC LIMIT 100`, sinceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []childEvent
	for rows.Next() {
		var e childEvent
		if err := rows.Scan(&e.ID, &e.FromPhase, &e.ToPhase, &e.EventType); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

// countActiveDispatches counts dispatches with status in ('spawned', 'running') in a child DB.
func countActiveDispatches(ctx context.Context, db *sql.DB) (int, error) {
	var count int
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM dispatches WHERE status IN ('spawned', 'running')`,
	).Scan(&count)
	return count, err
}

// loadCursors reads relay cursors from the state table.
func (r *Relay) loadCursors(ctx context.Context) (map[string]int64, error) {
	cursors := make(map[string]int64)
	children, err := r.store.GetChildren(ctx, r.portfolioID)
	if err != nil {
		return cursors, err
	}
	for _, child := range children {
		payload, err := r.stateStore.Get(ctx, "relay-cursor", child.ProjectDir)
		if err != nil {
			continue // no cursor yet
		}
		// payload is JSON string like "\"42\""
		var s string
		if err := json.Unmarshal(payload, &s); err != nil {
			continue
		}
		val, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			continue
		}
		cursors[child.ProjectDir] = val
	}
	return cursors, nil
}
