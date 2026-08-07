package main

import (
	"context"
	"database/sql"
	"log/slog"

	"github.com/mistakeknot/intercore/internal/budget"
	"github.com/mistakeknot/intercore/internal/dispatch"
	"github.com/mistakeknot/intercore/internal/event"
)

// newDispatchRecorder returns a recorder that persists dispatch status
// transitions to the event store. Recording is post-commit and
// fire-and-forget (errors are Debug-logged, never propagated): the
// transition itself is already durable in the dispatches table — this
// recorder exists so the transition also appears in the event stream.
func newDispatchRecorder(db *sql.DB) dispatch.DispatchEventRecorder {
	evStore := event.NewStore(db)
	return func(dispatchID, runID, fromStatus, toStatus string) {
		if err := evStore.AddDispatchEvent(context.Background(), dispatchID, runID, fromStatus, toStatus, "status_change", "", nil); err != nil {
			slog.Debug("dispatch event record failed", "dispatch", dispatchID, "error", err)
		}
	}
}

// newBudgetRecorder returns a recorder that persists budget.warning /
// budget.exceeded threshold crossings as run-scoped events, so a crossed
// budget leaves a durable record instead of only an in-memory Result.
func newBudgetRecorder(db *sql.DB) budget.EventRecorder {
	evStore := event.NewStore(db)
	return func(ctx context.Context, runID, eventType, reason string) error {
		return evStore.AddBudgetEvent(ctx, runID, eventType, reason)
	}
}
