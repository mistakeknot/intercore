package phase

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

func phaseReplayPayload(e *PhaseEvent) string {
	out := map[string]interface{}{
		"event_type": e.EventType,
		"from_phase": e.FromPhase,
		"to_phase":   e.ToPhase,
	}
	if e.GateResult != nil {
		out["gate_result"] = *e.GateResult
	}
	if e.GateTier != nil {
		out["gate_tier"] = *e.GateTier
	}
	if e.Reason != nil {
		out["reason"] = *e.Reason
	}
	if e.EnvelopeJSON != nil && *e.EnvelopeJSON != "" {
		out["envelope_json"] = *e.EnvelopeJSON
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "{}"
	}
	return string(b)
}

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
