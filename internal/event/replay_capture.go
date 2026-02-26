package event

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

func insertReplayInput(
	ctx context.Context,
	exec func(context.Context, string, ...interface{}) (sql.Result, error),
	runID, kind, key, payload, artifactRef, eventSource string,
	eventID *int64,
) error {
	if runID == "" {
		return nil
	}
	if kind == "" {
		kind = "unknown"
	}
	if payload == "" {
		payload = "{}"
	}
	_, err := exec(ctx, `
		INSERT INTO run_replay_inputs (
			run_id, kind, input_key, payload, artifact_ref, event_source, event_id
		) VALUES (?, ?, NULLIF(?, ''), ?, NULLIF(?, ''), NULLIF(?, ''), ?)`,
		runID, kind, key, payload, artifactRef, eventSource, eventID,
	)
	if err != nil {
		if strings.Contains(err.Error(), "no such table: run_replay_inputs") ||
			strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
			return nil
		}
		return fmt.Errorf("insert replay input: %w", err)
	}
	return nil
}

func dispatchReplayPayload(dispatchID, fromStatus, toStatus, eventType, reason string, envelope *EventEnvelope) string {
	out := map[string]interface{}{
		"dispatch_id": dispatchID,
		"event_type":  eventType,
		"from_status": fromStatus,
		"to_status":   toStatus,
	}
	if reason != "" {
		out["reason"] = reason
	}
	if envelope != nil {
		out["envelope"] = envelope
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func coordinationReplayPayload(eventType, lockID, owner, pattern, scope, reason string, envelope *EventEnvelope) string {
	out := map[string]interface{}{
		"event_type": eventType,
		"lock_id":    lockID,
		"owner":      owner,
		"pattern":    pattern,
		"scope":      scope,
	}
	if reason != "" {
		out["reason"] = reason
	}
	if envelope != nil {
		out["envelope"] = envelope
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "{}"
	}
	return string(b)
}
