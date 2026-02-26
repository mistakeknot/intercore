package replay

import (
	"context"
	"database/sql"
	"fmt"
)

// Input is a recorded nondeterministic input associated with a run.
type Input struct {
	ID          int64   `json:"id"`
	RunID       string  `json:"run_id"`
	Kind        string  `json:"kind"`
	Key         string  `json:"key"`
	Payload     string  `json:"payload"`
	ArtifactRef *string `json:"artifact_ref,omitempty"`
	EventSource string  `json:"event_source,omitempty"`
	EventID     *int64  `json:"event_id,omitempty"`
	CreatedAt   int64   `json:"created_at"`
}

// Store provides replay input persistence and query methods.
type Store struct {
	db *sql.DB
}

// New creates a replay input store.
func New(db *sql.DB) *Store {
	return &Store{db: db}
}

// AddInput records a nondeterministic input for a run.
func (s *Store) AddInput(ctx context.Context, in *Input) (int64, error) {
	if in == nil {
		return 0, fmt.Errorf("replay add input: nil input")
	}
	if in.RunID == "" {
		return 0, fmt.Errorf("replay add input: run_id is required")
	}
	if in.Kind == "" {
		return 0, fmt.Errorf("replay add input: kind is required")
	}
	payload := in.Payload
	if payload == "" {
		payload = "{}"
	}

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO run_replay_inputs (
			run_id, kind, input_key, payload, artifact_ref, event_source, event_id
		) VALUES (?, ?, NULLIF(?, ''), ?, NULLIF(?, ''), NULLIF(?, ''), ?)`,
		in.RunID, in.Kind, in.Key, payload, ptrStr(in.ArtifactRef), in.EventSource, in.EventID,
	)
	if err != nil {
		return 0, fmt.Errorf("replay add input: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("replay add input id: %w", err)
	}
	return id, nil
}

// ListInputs returns replay inputs for a run in deterministic order.
func (s *Store) ListInputs(ctx context.Context, runID string, limit int) ([]*Input, error) {
	if runID == "" {
		return nil, fmt.Errorf("replay list inputs: run_id is required")
	}
	if limit <= 0 {
		limit = 1000
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, run_id, kind, COALESCE(input_key, ''), payload,
			artifact_ref, COALESCE(event_source, ''), event_id, created_at
		FROM run_replay_inputs
		WHERE run_id = ?
		ORDER BY created_at ASC, id ASC
		LIMIT ?`,
		runID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("replay list inputs: %w", err)
	}
	defer rows.Close()

	var inputs []*Input
	for rows.Next() {
		var (
			item     Input
			artifact sql.NullString
			source   string
			eventID  sql.NullInt64
		)
		if err := rows.Scan(
			&item.ID, &item.RunID, &item.Kind, &item.Key, &item.Payload,
			&artifact, &source, &eventID, &item.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("replay list inputs scan: %w", err)
		}
		if artifact.Valid {
			item.ArtifactRef = &artifact.String
		}
		item.EventSource = source
		if eventID.Valid {
			v := eventID.Int64
			item.EventID = &v
		}
		inputs = append(inputs, &item)
	}
	return inputs, rows.Err()
}

func ptrStr(v *string) interface{} {
	if v == nil {
		return nil
	}
	return *v
}
