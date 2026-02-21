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
	EventChildAdvanced     = "child_advanced"
	EventChildCompleted    = "child_completed"
	EventUpstreamChanged   = "upstream_changed"
)

// Relay polls child project databases and relays events to the portfolio run.
type Relay struct {
	portfolioID string
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

	for {
		if err := r.poll(ctx, cursors); err != nil {
			fmt.Fprintf(r.logw, "[relay] poll error: %v\n", err)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(r.interval):
		}
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

		for _, evt := range events {
			// Relay to portfolio's phase_events
			relayReason := fmt.Sprintf("relay:%s", child.ProjectDir)
			eventType := EventChildAdvanced
			if evt.EventType == phase.EventCancel {
				eventType = EventChildCompleted
			}

			r.store.AddEvent(ctx, &phase.PhaseEvent{
				RunID:     r.portfolioID,
				FromPhase: evt.FromPhase,
				ToPhase:   evt.ToPhase,
				EventType: eventType,
				Reason:    &relayReason,
			})

			fmt.Fprintf(r.logw, "[relay] %s: %s %s→%s\n", child.ProjectDir, eventType, evt.FromPhase, evt.ToPhase)

			// Update cursor
			if evt.ID > cursor {
				cursor = evt.ID
			}

			// Check dependencies: did this child advance past an upstream boundary?
			for _, dep := range deps {
				if dep.UpstreamProject == child.ProjectDir {
					reason := fmt.Sprintf("%s:%s→%s", EventUpstreamChanged, evt.FromPhase, evt.ToPhase)
					r.store.AddEvent(ctx, &phase.PhaseEvent{
						RunID:     r.portfolioID,
						FromPhase: evt.FromPhase,
						ToPhase:   evt.ToPhase,
						EventType: EventUpstreamChanged,
						Reason:    &reason,
					})
				}
			}
		}

		cursors[child.ProjectDir] = cursor
		r.saveCursor(ctx, child.ProjectDir, cursor)

		// Count active dispatches in child project
		active, err := countActiveDispatches(ctx, childDB)
		if err == nil {
			totalActive += active
		}
	}

	// Write active dispatch count to state table
	r.stateStore.Set(ctx, "active-dispatch-count", r.portfolioID,
		json.RawMessage(strconv.Quote(strconv.Itoa(totalActive))), 0)

	return nil
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

// saveCursor persists a relay cursor to the state table.
func (r *Relay) saveCursor(ctx context.Context, projectDir string, cursor int64) {
	r.stateStore.Set(ctx, "relay-cursor", projectDir,
		json.RawMessage(strconv.Quote(strconv.FormatInt(cursor, 10))), 0)
}
