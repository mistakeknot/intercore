package budget

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/mistakeknot/interverse/infra/intercore/internal/dispatch"
)

// EventCostDiscrepancy is emitted when billed tokens differ from self-reported tokens.
const EventCostDiscrepancy = "cost.reconciliation_discrepancy"

// Reconciliation holds the result of comparing reported vs billed tokens.
type Reconciliation struct {
	ID          int64
	RunID       string
	DispatchID  string // empty = run-level
	ReportedIn  int64
	ReportedOut int64
	BilledIn    int64
	BilledOut   int64
	DeltaIn     int64 // billed - reported
	DeltaOut    int64 // billed - reported
	Source      string
	CreatedAt   time.Time
}

// ReconcileStore handles cost reconciliation CRUD.
type ReconcileStore struct {
	db            *sql.DB
	dispatchStore *dispatch.Store
}

// NewReconcileStore creates a reconciliation store.
func NewReconcileStore(db *sql.DB, ds *dispatch.Store) *ReconcileStore {
	return &ReconcileStore{db: db, dispatchStore: ds}
}

// Reconcile compares billed tokens against self-reported tokens for a run or dispatch.
// If dispatchID is empty, reconciles at run level (aggregating all dispatches).
// Records the result and emits EventCostDiscrepancy if any delta is non-zero.
func (s *ReconcileStore) Reconcile(ctx context.Context, runID, dispatchID string, billedIn, billedOut int64, source string, recorder EventRecorder) (*Reconciliation, error) {
	var reportedIn, reportedOut int64

	if dispatchID != "" {
		// Dispatch-level: read single dispatch tokens
		d, err := s.dispatchStore.Get(ctx, dispatchID)
		if err != nil {
			return nil, fmt.Errorf("reconcile: get dispatch: %w", err)
		}
		reportedIn = int64(d.InputTokens)
		reportedOut = int64(d.OutputTokens)
	} else {
		// Run-level: aggregate across all dispatches
		agg, err := s.dispatchStore.AggregateTokens(ctx, runID)
		if err != nil {
			return nil, fmt.Errorf("reconcile: aggregate tokens: %w", err)
		}
		reportedIn = agg.TotalIn
		reportedOut = agg.TotalOut
	}

	deltaIn := billedIn - reportedIn
	deltaOut := billedOut - reportedOut

	if source == "" {
		source = "manual"
	}

	result, err := s.db.ExecContext(ctx, `
		INSERT INTO cost_reconciliations (run_id, dispatch_id, reported_in, reported_out, billed_in, billed_out, delta_in, delta_out, source)
		VALUES (?, NULLIF(?, ''), ?, ?, ?, ?, ?, ?, ?)`,
		runID, dispatchID, reportedIn, reportedOut, billedIn, billedOut, deltaIn, deltaOut, source,
	)
	if err != nil {
		return nil, fmt.Errorf("reconcile: insert: %w", err)
	}

	id, _ := result.LastInsertId()

	rec := &Reconciliation{
		ID:          id,
		RunID:       runID,
		DispatchID:  dispatchID,
		ReportedIn:  reportedIn,
		ReportedOut: reportedOut,
		BilledIn:    billedIn,
		BilledOut:   billedOut,
		DeltaIn:     deltaIn,
		DeltaOut:    deltaOut,
		Source:      source,
	}

	// Emit discrepancy event if deltas are non-zero
	if (deltaIn != 0 || deltaOut != 0) && recorder != nil {
		reason := fmt.Sprintf("token discrepancy: in=%+d out=%+d (reported=%d/%d, billed=%d/%d)",
			deltaIn, deltaOut, reportedIn, reportedOut, billedIn, billedOut)
		if dispatchID != "" {
			reason = fmt.Sprintf("dispatch %s: %s", dispatchID, reason)
		}
		recorder(ctx, runID, EventCostDiscrepancy, reason)
	}

	return rec, nil
}

// List returns reconciliations for a run, ordered by created_at desc.
func (s *ReconcileStore) List(ctx context.Context, runID string, limit int) ([]Reconciliation, error) {
	if limit <= 0 {
		limit = 100
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, run_id, COALESCE(dispatch_id, ''), reported_in, reported_out,
		       billed_in, billed_out, delta_in, delta_out, source, created_at
		FROM cost_reconciliations
		WHERE run_id = ?
		ORDER BY id DESC
		LIMIT ?`, runID, limit)
	if err != nil {
		return nil, fmt.Errorf("reconcile list: %w", err)
	}
	defer rows.Close()

	var recs []Reconciliation
	for rows.Next() {
		var r Reconciliation
		var createdAt int64
		if err := rows.Scan(&r.ID, &r.RunID, &r.DispatchID, &r.ReportedIn, &r.ReportedOut,
			&r.BilledIn, &r.BilledOut, &r.DeltaIn, &r.DeltaOut, &r.Source, &createdAt); err != nil {
			return nil, fmt.Errorf("reconcile list: scan: %w", err)
		}
		r.CreatedAt = time.Unix(createdAt, 0)
		recs = append(recs, r)
	}
	return recs, rows.Err()
}
