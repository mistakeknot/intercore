package dispatch

import (
	"context"
	"database/sql"
	"fmt"
)

// ConflictTelemetry holds aggregated TOCTOU conflict metrics for a scope.
type ConflictTelemetry struct {
	TotalDispatches    int    `json:"total_dispatches"`
	WithConflicts      int    `json:"with_conflicts"`
	WriteWriteCount    int    `json:"write_write_count"`
	TotalRetries       int    `json:"total_retries"`
	QuarantinedCount   int    `json:"quarantined_count"`
	ConflictRate       float64 `json:"conflict_rate"`       // WithConflicts / TotalDispatches
	AvgRetriesPerMerge float64 `json:"avg_retries_per_merge"` // TotalRetries / completed dispatches
}

// ConflictTelemetryByType holds per-conflict-type breakdown.
type ConflictTelemetryByType struct {
	ConflictType string `json:"conflict_type"`
	Count        int    `json:"count"`
	TotalRetries int    `json:"total_retries"`
}

// TelemetryStore provides conflict telemetry queries.
type TelemetryStore struct {
	db *sql.DB
}

// NewTelemetryStore creates a telemetry store.
func NewTelemetryStore(db *sql.DB) *TelemetryStore {
	return &TelemetryStore{db: db}
}

// GetConflictTelemetry returns aggregated conflict metrics for a scope (run).
// If scopeID is empty, aggregates across all dispatches.
func (s *TelemetryStore) GetConflictTelemetry(ctx context.Context, scopeID string) (*ConflictTelemetry, error) {
	var query string
	var args []interface{}

	if scopeID != "" {
		query = `
			SELECT
				COUNT(*) AS total,
				COUNT(conflict_type) AS with_conflicts,
				COALESCE(SUM(CASE WHEN conflict_type = 'write_write' THEN 1 ELSE 0 END), 0) AS write_write,
				COALESCE(SUM(retry_count), 0) AS total_retries,
				COALESCE(SUM(CASE WHEN quarantine_reason IS NOT NULL THEN 1 ELSE 0 END), 0) AS quarantined
			FROM dispatches
			WHERE scope_id = ?`
		args = []interface{}{scopeID}
	} else {
		query = `
			SELECT
				COUNT(*) AS total,
				COUNT(conflict_type) AS with_conflicts,
				COALESCE(SUM(CASE WHEN conflict_type = 'write_write' THEN 1 ELSE 0 END), 0) AS write_write,
				COALESCE(SUM(retry_count), 0) AS total_retries,
				COALESCE(SUM(CASE WHEN quarantine_reason IS NOT NULL THEN 1 ELSE 0 END), 0) AS quarantined
			FROM dispatches`
	}

	t := &ConflictTelemetry{}
	err := s.db.QueryRowContext(ctx, query, args...).Scan(
		&t.TotalDispatches, &t.WithConflicts, &t.WriteWriteCount,
		&t.TotalRetries, &t.QuarantinedCount,
	)
	if err != nil {
		return nil, fmt.Errorf("conflict telemetry: %w", err)
	}

	if t.TotalDispatches > 0 {
		t.ConflictRate = float64(t.WithConflicts) / float64(t.TotalDispatches)
	}

	// Average retries per completed dispatch
	var completed int
	if scopeID != "" {
		err = s.db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM dispatches WHERE scope_id = ? AND status = 'completed'",
			scopeID).Scan(&completed)
	} else {
		err = s.db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM dispatches WHERE status = 'completed'").Scan(&completed)
	}
	if err == nil && completed > 0 {
		t.AvgRetriesPerMerge = float64(t.TotalRetries) / float64(completed)
	}

	return t, nil
}

// GetConflictsByType returns per-conflict-type breakdown.
func (s *TelemetryStore) GetConflictsByType(ctx context.Context, scopeID string) ([]ConflictTelemetryByType, error) {
	var query string
	var args []interface{}

	if scopeID != "" {
		query = `
			SELECT conflict_type, COUNT(*) AS cnt, COALESCE(SUM(retry_count), 0)
			FROM dispatches
			WHERE scope_id = ? AND conflict_type IS NOT NULL
			GROUP BY conflict_type
			ORDER BY cnt DESC`
		args = []interface{}{scopeID}
	} else {
		query = `
			SELECT conflict_type, COUNT(*) AS cnt, COALESCE(SUM(retry_count), 0)
			FROM dispatches
			WHERE conflict_type IS NOT NULL
			GROUP BY conflict_type
			ORDER BY cnt DESC`
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("conflicts by type: %w", err)
	}
	defer rows.Close()

	var results []ConflictTelemetryByType
	for rows.Next() {
		var cbt ConflictTelemetryByType
		if err := rows.Scan(&cbt.ConflictType, &cbt.Count, &cbt.TotalRetries); err != nil {
			return nil, fmt.Errorf("conflicts by type scan: %w", err)
		}
		results = append(results, cbt)
	}
	return results, rows.Err()
}

// RecordConflict updates a dispatch with conflict information.
// This is called by the merge pipeline when a write-set conflict is detected.
func (s *TelemetryStore) RecordConflict(ctx context.Context, dispatchID, conflictType, quarantineReason string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE dispatches
		SET conflict_type = ?, quarantine_reason = ?, retry_count = retry_count + 1
		WHERE id = ?`,
		conflictType, nilIfEmpty(quarantineReason), dispatchID,
	)
	if err != nil {
		return fmt.Errorf("record conflict: %w", err)
	}
	return nil
}

// IncrementRetry bumps the retry count for a dispatch.
func (s *TelemetryStore) IncrementRetry(ctx context.Context, dispatchID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE dispatches SET retry_count = retry_count + 1 WHERE id = ?`,
		dispatchID,
	)
	if err != nil {
		return fmt.Errorf("increment retry: %w", err)
	}
	return nil
}

func nilIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}
