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

// ListEvents returns unified events for a run, merging phase_events,
// dispatch_events, and discovery_events, ordered by timestamp. Uses separate
// per-table cursors to avoid conflating independent AUTOINCREMENT ID spaces.
func (s *Store) ListEvents(ctx context.Context, runID string, sincePhaseID, sinceDispatchID, sinceDiscoveryID int64, limit int) ([]Event, error) {
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
		UNION ALL
		-- discovery_events: discovery_id AS run_id is for column alignment only
		SELECT id, COALESCE(discovery_id, '') AS run_id, 'discovery' AS source, event_type,
			from_status, to_status, COALESCE(payload, '{}') AS reason, created_at
		FROM discovery_events
		WHERE id > ?
		ORDER BY created_at ASC, source ASC, id ASC
		LIMIT ?`,
		runID, sincePhaseID,
		runID, runID, sinceDispatchID,
		sinceDiscoveryID,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()

	return scanEvents(rows)
}

// ListAllEvents returns events across all runs, merging all three tables.
func (s *Store) ListAllEvents(ctx context.Context, sincePhaseID, sinceDispatchID, sinceDiscoveryID int64, limit int) ([]Event, error) {
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
		UNION ALL
		-- discovery_events: discovery_id AS run_id is for column alignment only
		SELECT id, COALESCE(discovery_id, '') AS run_id, 'discovery' AS source, event_type,
			from_status, to_status, COALESCE(payload, '{}') AS reason, created_at
		FROM discovery_events
		WHERE id > ?
		ORDER BY created_at ASC, source ASC, id ASC
		LIMIT ?`,
		sincePhaseID, sinceDispatchID, sinceDiscoveryID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list all events: %w", err)
	}
	defer rows.Close()

	return scanEvents(rows)
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

// MaxDiscoveryEventID returns the highest discovery_events.id (for cursor init).
func (s *Store) MaxDiscoveryEventID(ctx context.Context) (int64, error) {
	var id sql.NullInt64
	err := s.db.QueryRowContext(ctx, "SELECT MAX(id) FROM discovery_events").Scan(&id)
	if err != nil {
		return 0, err
	}
	if !id.Valid {
		return 0, nil
	}
	return id.Int64, nil
}

// AddInterspectEvent records a human correction or agent dispatch signal.
func (s *Store) AddInterspectEvent(ctx context.Context, runID, agentName, eventType, overrideReason, contextJSON, sessionID, projectDir string) (int64, error) {
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO interspect_events (run_id, agent_name, event_type, override_reason, context_json, session_id, project_dir)
		VALUES (NULLIF(?, ''), ?, ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''))`,
		runID, agentName, eventType, overrideReason, contextJSON, sessionID, projectDir,
	)
	if err != nil {
		return 0, fmt.Errorf("add interspect event: %w", err)
	}
	return result.LastInsertId()
}

// ListInterspectEvents returns interspect events, optionally filtered by agent name.
func (s *Store) ListInterspectEvents(ctx context.Context, agentName string, since int64, limit int) ([]InterspectEvent, error) {
	if limit <= 0 {
		limit = 1000
	}

	var rows *sql.Rows
	var err error
	if agentName != "" {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, COALESCE(run_id, '') AS run_id, agent_name, event_type,
				COALESCE(override_reason, '') AS override_reason,
				COALESCE(context_json, '') AS context_json,
				COALESCE(session_id, '') AS session_id,
				COALESCE(project_dir, '') AS project_dir,
				created_at
			FROM interspect_events
			WHERE agent_name = ? AND id > ?
			ORDER BY created_at ASC, id ASC
			LIMIT ?`,
			agentName, since, limit,
		)
	} else {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, COALESCE(run_id, '') AS run_id, agent_name, event_type,
				COALESCE(override_reason, '') AS override_reason,
				COALESCE(context_json, '') AS context_json,
				COALESCE(session_id, '') AS session_id,
				COALESCE(project_dir, '') AS project_dir,
				created_at
			FROM interspect_events
			WHERE id > ?
			ORDER BY created_at ASC, id ASC
			LIMIT ?`,
			since, limit,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("list interspect events: %w", err)
	}
	defer rows.Close()

	var events []InterspectEvent
	for rows.Next() {
		var e InterspectEvent
		var createdAt int64
		if err := rows.Scan(&e.ID, &e.RunID, &e.AgentName, &e.EventType,
			&e.OverrideReason, &e.ContextJSON, &e.SessionID, &e.ProjectDir, &createdAt); err != nil {
			return nil, fmt.Errorf("interspect events scan: %w", err)
		}
		e.Timestamp = time.Unix(createdAt, 0)
		events = append(events, e)
	}
	return events, rows.Err()
}

// MaxInterspectEventID returns the highest interspect_events.id (for cursor tracking).
func (s *Store) MaxInterspectEventID(ctx context.Context) (int64, error) {
	var id sql.NullInt64
	err := s.db.QueryRowContext(ctx, "SELECT MAX(id) FROM interspect_events").Scan(&id)
	if err != nil {
		return 0, err
	}
	if !id.Valid {
		return 0, nil
	}
	return id.Int64, nil
}

func scanEvents(rows *sql.Rows) ([]Event, error) {
	var events []Event
	for rows.Next() {
		var e Event
		var createdAt int64
		if err := rows.Scan(&e.ID, &e.RunID, &e.Source, &e.Type,
			&e.FromState, &e.ToState, &e.Reason, &createdAt); err != nil {
			return nil, fmt.Errorf("events scan: %w", err)
		}
		e.Timestamp = time.Unix(createdAt, 0)
		events = append(events, e)
	}
	return events, rows.Err()
}
