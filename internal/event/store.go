package event

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/mistakeknot/intercore/pkg/redaction"
)

// Store provides event read/write operations.
type Store struct {
	db        *sql.DB
	redactCfg *redaction.Config
}

// NewStore creates an event store.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// SetRedactionConfig enables automatic redaction of user-content fields
// before persistence. Pass nil to disable.
func (s *Store) SetRedactionConfig(cfg *redaction.Config) {
	s.redactCfg = cfg
}

// redactStr applies redaction to a string if a config is set.
// Returns the input unchanged if redaction is disabled.
func (s *Store) redactStr(input string) string {
	if s.redactCfg == nil || input == "" {
		return input
	}
	output, _ := redaction.Redact(input, *s.redactCfg)
	return output
}

// redactStrPtr applies redaction to a *string if a config is set.
func (s *Store) redactStrPtr(input *string) *string {
	if s.redactCfg == nil || input == nil || *input == "" {
		return input
	}
	output, _ := redaction.Redact(*input, *s.redactCfg)
	return &output
}

// AddDispatchEvent records a dispatch lifecycle event.
func (s *Store) AddDispatchEvent(ctx context.Context, dispatchID, runID, fromStatus, toStatus, eventType, reason string, envelope *EventEnvelope) error {
	reason = s.redactStr(reason)

	if envelope == nil {
		envelope = s.defaultDispatchEnvelope(ctx, dispatchID, runID, fromStatus, toStatus)
	}
	envelopeJSON, err := MarshalEnvelopeJSON(envelope)
	if err != nil {
		return fmt.Errorf("add dispatch event: marshal envelope: %w", err)
	}
	envelopeJSON = s.redactStrPtr(envelopeJSON)

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO dispatch_events (
			dispatch_id, run_id, from_status, to_status, event_type, reason, envelope_json
		) VALUES (?, NULLIF(?, ''), ?, ?, ?, NULLIF(?, ''), ?)`,
		dispatchID, runID, fromStatus, toStatus, eventType, reason, envelopeJSON,
	)
	if err != nil {
		return fmt.Errorf("add dispatch event: %w", err)
	}

	var eventIDPtr *int64
	artifactRef := ""
	if eventID, idErr := res.LastInsertId(); idErr == nil && eventID > 0 {
		eventIDPtr = &eventID
		artifactRef = fmt.Sprintf("dispatch_event:%d", eventID)
	}
	if err := insertReplayInput(
		ctx,
		s.db.ExecContext,
		runID,
		"external",
		"dispatch_transition",
		dispatchReplayPayload(dispatchID, fromStatus, toStatus, eventType, reason, envelope),
		artifactRef,
		"dispatch",
		eventIDPtr,
	); err != nil {
		return fmt.Errorf("add dispatch event replay input: %w", err)
	}
	return nil
}

// ListEvents returns unified events for a run, merging phase_events,
// dispatch_events, coordination_events, and review_events, ordered by
// timestamp. Uses separate per-table cursors to avoid conflating independent
// AUTOINCREMENT ID spaces.
func (s *Store) ListEvents(ctx context.Context, runID string, sincePhaseID, sinceDispatchID, sinceDiscoveryID, sinceReviewID int64, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 1000
	}

	// Note: discovery_events are system-level (no run_id column) and excluded
	// from run-scoped queries. Use ListAllEvents for cross-run streams including
	// discovery events. The sinceDiscoveryID param is accepted but unused here
	// to keep the signature aligned with ListAllEvents.
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, run_id, 'phase' AS source, event_type, from_phase, to_phase,
			COALESCE(reason, '') AS reason, COALESCE(envelope_json, '') AS envelope_json, created_at
		FROM phase_events
		WHERE run_id = ? AND id > ?
		UNION ALL
		SELECT id, COALESCE(run_id, '') AS run_id, 'dispatch' AS source, event_type,
			from_status, to_status, COALESCE(reason, '') AS reason, COALESCE(envelope_json, '') AS envelope_json, created_at
		FROM dispatch_events
		WHERE (run_id = ? OR ? = '') AND id > ?
		UNION ALL
		SELECT id, COALESCE(run_id, '') AS run_id, 'coordination' AS source, event_type,
			owner, pattern, COALESCE(reason, '') AS reason, COALESCE(envelope_json, '') AS envelope_json, created_at
		FROM coordination_events
		WHERE (run_id = ? OR ? = '') AND id > 0
		UNION ALL
		SELECT id, COALESCE(run_id, '') AS run_id, 'review' AS source, 'disagreement_resolved' AS event_type,
			finding_id, resolution, COALESCE(agents_json, '{}') AS reason, '' AS envelope_json, created_at
		FROM review_events
		WHERE (run_id = ? OR ? = '') AND id > ?
		ORDER BY created_at ASC, source ASC, id ASC
		LIMIT ?`,
		runID, sincePhaseID,
		runID, runID, sinceDispatchID,
		runID, runID,
		runID, runID, sinceReviewID,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()

	return scanEvents(rows)
}

// ListAllEvents returns events across all runs, merging all five event tables.
func (s *Store) ListAllEvents(ctx context.Context, sincePhaseID, sinceDispatchID, sinceDiscoveryID, sinceReviewID int64, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 1000
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, run_id, 'phase' AS source, event_type, from_phase, to_phase,
			COALESCE(reason, '') AS reason, COALESCE(envelope_json, '') AS envelope_json, created_at
		FROM phase_events
		WHERE id > ?
		UNION ALL
		SELECT id, COALESCE(run_id, '') AS run_id, 'dispatch' AS source, event_type,
			from_status, to_status, COALESCE(reason, '') AS reason, COALESCE(envelope_json, '') AS envelope_json, created_at
		FROM dispatch_events
		WHERE id > ?
		UNION ALL
		-- discovery_events: discovery_id AS run_id is for column alignment only
		SELECT id, COALESCE(discovery_id, '') AS run_id, 'discovery' AS source, event_type,
			from_status, to_status, COALESCE(payload, '{}') AS reason, '' AS envelope_json, created_at
		FROM discovery_events
		WHERE id > ?
		UNION ALL
		SELECT id, COALESCE(run_id, '') AS run_id, 'coordination' AS source, event_type,
			owner, pattern, COALESCE(reason, '') AS reason, COALESCE(envelope_json, '') AS envelope_json, created_at
		FROM coordination_events
		WHERE id > 0
		UNION ALL
		SELECT id, COALESCE(run_id, '') AS run_id, 'review' AS source, 'disagreement_resolved' AS event_type,
			finding_id, resolution, COALESCE(agents_json, '{}') AS reason, '' AS envelope_json, created_at
		FROM review_events
		WHERE id > ?
		ORDER BY created_at ASC, source ASC, id ASC
		LIMIT ?`,
		sincePhaseID, sinceDispatchID, sinceDiscoveryID, sinceReviewID, limit,
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

// AddCoordinationEvent records a coordination lock lifecycle event.
func (s *Store) AddCoordinationEvent(ctx context.Context, eventType, lockID, owner, pattern, scope, reason, runID string, envelope *EventEnvelope) error {
	reason = s.redactStr(reason)

	if envelope == nil {
		envelope = defaultCoordinationEnvelope(eventType, lockID, pattern, scope, runID)
	}
	envelopeJSON, err := MarshalEnvelopeJSON(envelope)
	if err != nil {
		return fmt.Errorf("add coordination event: marshal envelope: %w", err)
	}
	envelopeJSON = s.redactStrPtr(envelopeJSON)

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO coordination_events (
			lock_id, run_id, event_type, owner, pattern, scope, reason, envelope_json
		) VALUES (?, NULLIF(?, ''), ?, ?, ?, ?, NULLIF(?, ''), ?)`,
		lockID, runID, eventType, owner, pattern, scope, reason, envelopeJSON,
	)
	if err != nil {
		return fmt.Errorf("add coordination event: %w", err)
	}

	var eventIDPtr *int64
	artifactRef := ""
	if eventID, idErr := res.LastInsertId(); idErr == nil && eventID > 0 {
		eventIDPtr = &eventID
		artifactRef = fmt.Sprintf("coordination_event:%d", eventID)
	}
	if err := insertReplayInput(
		ctx,
		s.db.ExecContext,
		runID,
		"external",
		"coordination_transition",
		coordinationReplayPayload(eventType, lockID, owner, pattern, scope, reason, envelope),
		artifactRef,
		"coordination",
		eventIDPtr,
	); err != nil {
		return fmt.Errorf("add coordination event replay input: %w", err)
	}
	return nil
}

// MaxCoordinationEventID returns the highest coordination_events.id (for cursor tracking).
func (s *Store) MaxCoordinationEventID(ctx context.Context) (int64, error) {
	var id sql.NullInt64
	err := s.db.QueryRowContext(ctx, "SELECT MAX(id) FROM coordination_events").Scan(&id)
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
	overrideReason = s.redactStr(overrideReason)
	contextJSON = s.redactStr(contextJSON)

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

// AddReviewEvent records a disagreement resolution or execution defect event.
func (s *Store) AddReviewEvent(ctx context.Context, runID, findingID, agentsJSON, resolution, dismissalReason, chosenSeverity, impact, eventType, sessionID, projectDir string) (int64, error) {
	agentsJSON = s.redactStr(agentsJSON)
	resolution = s.redactStr(resolution)
	dismissalReason = s.redactStr(dismissalReason)
	impact = s.redactStr(impact)

	if eventType == "" {
		eventType = "disagreement_resolved"
	}

	result, err := s.db.ExecContext(ctx, `
		INSERT INTO review_events (run_id, finding_id, agents_json, resolution, dismissal_reason, chosen_severity, impact, event_type, session_id, project_dir)
		VALUES (NULLIF(?, ''), ?, ?, ?, NULLIF(?, ''), ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''))`,
		runID, findingID, agentsJSON, resolution, dismissalReason, chosenSeverity, impact, eventType, sessionID, projectDir,
	)
	if err != nil {
		return 0, fmt.Errorf("add review event: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}

	// Create replay input (consistent with dispatch/coordination pattern, per PRD F1)
	if runID != "" {
		payload := reviewReplayPayload(findingID, agentsJSON, resolution, dismissalReason, chosenSeverity, impact, eventType)
		if err := insertReplayInput(ctx, s.db.ExecContext, runID, "review_event", findingID, payload, "", SourceReview, &id); err != nil {
			return id, fmt.Errorf("add review event: replay input: %w", err)
		}
	}

	return id, nil
}

// ListReviewEvents returns review events since a cursor position.
func (s *Store) ListReviewEvents(ctx context.Context, since int64, limit int) ([]ReviewEvent, error) {
	if limit <= 0 {
		limit = 1000
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, COALESCE(run_id, '') AS run_id, finding_id, agents_json, resolution,
			COALESCE(dismissal_reason, '') AS dismissal_reason, chosen_severity, impact,
			COALESCE(event_type, 'disagreement_resolved') AS event_type,
			COALESCE(session_id, '') AS session_id,
			COALESCE(project_dir, '') AS project_dir,
			created_at
		FROM review_events
		WHERE id > ?
		ORDER BY created_at ASC, id ASC
		LIMIT ?`,
		since, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list review events: %w", err)
	}
	defer rows.Close()

	var events []ReviewEvent
	for rows.Next() {
		var e ReviewEvent
		var ts int64
		if err := rows.Scan(&e.ID, &e.RunID, &e.FindingID, &e.AgentsJSON, &e.Resolution,
			&e.DismissalReason, &e.ChosenSeverity, &e.Impact, &e.EventType,
			&e.SessionID, &e.ProjectDir, &ts); err != nil {
			return nil, fmt.Errorf("scan review event: %w", err)
		}
		e.Timestamp = time.Unix(ts, 0)
		events = append(events, e)
	}
	return events, rows.Err()
}

// MaxReviewEventID returns the highest review_events.id (for cursor init).
func (s *Store) MaxReviewEventID(ctx context.Context) (int64, error) {
	var id sql.NullInt64
	err := s.db.QueryRowContext(ctx, "SELECT MAX(id) FROM review_events").Scan(&id)
	if err != nil {
		return 0, err
	}
	if !id.Valid {
		return 0, nil
	}
	return id.Int64, nil
}

// AddIntentEvent records an intent submission event for audit trail.
func (s *Store) AddIntentEvent(ctx context.Context, intentType, beadID, idempotencyKey, sessionID, runID string, success bool, errorDetail string) error {
	errorDetail = s.redactStr(errorDetail)

	successInt := 0
	if success {
		successInt = 1
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO intent_events (
			intent_type, bead_id, idempotency_key, session_id, run_id, success, error_detail
		) VALUES (?, ?, ?, ?, NULLIF(?, ''), ?, NULLIF(?, ''))`,
		intentType, beadID, idempotencyKey, sessionID, runID, successInt, errorDetail,
	)
	if err != nil {
		return fmt.Errorf("add intent event: %w", err)
	}
	return nil
}

func scanEvents(rows *sql.Rows) ([]Event, error) {
	var events []Event
	for rows.Next() {
		var e Event
		var envelopeJSON string
		var createdAt int64
		if err := rows.Scan(&e.ID, &e.RunID, &e.Source, &e.Type,
			&e.FromState, &e.ToState, &e.Reason, &envelopeJSON, &createdAt); err != nil {
			return nil, fmt.Errorf("events scan: %w", err)
		}
		if envelopeJSON != "" {
			envelope, err := ParseEnvelopeJSON(envelopeJSON)
			if err == nil {
				e.Envelope = envelope
			}
		}
		e.Timestamp = time.Unix(createdAt, 0)
		events = append(events, e)
	}
	return events, rows.Err()
}

func (s *Store) defaultDispatchEnvelope(ctx context.Context, dispatchID, runID, fromStatus, toStatus string) *EventEnvelope {
	// Read propagated trace context from environment
	envTraceID := os.Getenv("IC_TRACE_ID")
	envSpanID := os.Getenv("IC_SPAN_ID")
	envParentSpanID := os.Getenv("IC_PARENT_SPAN_ID")

	traceID := envTraceID
	if traceID == "" {
		traceID = runID
		if traceID == "" {
			traceID = dispatchID
		}
	}

	capability := ""
	if runID != "" {
		capability = "run:" + runID
	} else if dispatchID != "" {
		capability = "dispatch:" + dispatchID
	}

	spanID := envSpanID
	if spanID == "" {
		spanID = fmt.Sprintf("dispatch:%s:%d", dispatchID, time.Now().UnixNano())
	}

	envelope := &EventEnvelope{
		PolicyVersion:      "dispatch-lifecycle/v2",
		CallerIdentity:     "dispatch.store",
		CapabilityScope:    capability,
		TraceID:            traceID,
		SpanID:             spanID,
		ParentSpanID:       envParentSpanID,
		InputArtifactRefs:  statusRef(fromStatus),
		OutputArtifactRefs: statusRef(toStatus),
	}

	if dispatchID == "" {
		return envelope
	}

	var requested, effective sql.NullString
	err := s.db.QueryRowContext(ctx,
		"SELECT sandbox_spec, sandbox_effective FROM dispatches WHERE id = ?",
		dispatchID,
	).Scan(&requested, &effective)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return envelope
	}
	if requested.Valid {
		envelope.RequestedSandbox = requested.String
	}
	if effective.Valid {
		envelope.EffectiveSandbox = effective.String
	}
	return envelope
}

func defaultCoordinationEnvelope(eventType, lockID, pattern, scope, runID string) *EventEnvelope {
	// Read propagated trace context from environment
	envTraceID := os.Getenv("IC_TRACE_ID")
	envSpanID := os.Getenv("IC_SPAN_ID")
	envParentSpanID := os.Getenv("IC_PARENT_SPAN_ID")

	traceID := envTraceID
	if traceID == "" {
		traceID = runID
		if traceID == "" {
			traceID = lockID
		}
	}

	spanID := envSpanID
	if spanID == "" {
		spanID = fmt.Sprintf("coordination:%s:%d", eventType, time.Now().UnixNano())
	}

	return &EventEnvelope{
		PolicyVersion:      "coordination/v1",
		CallerIdentity:     "coordination.store",
		CapabilityScope:    "scope:" + scope,
		TraceID:            traceID,
		SpanID:             spanID,
		ParentSpanID:       envParentSpanID,
		InputArtifactRefs:  statusRef(pattern),
		OutputArtifactRefs: statusRef(lockID),
	}
}

func statusRef(v string) []string {
	if v == "" {
		return nil
	}
	return []string{v}
}
