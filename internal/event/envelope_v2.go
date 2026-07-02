package event

import (
	"encoding/json"
	"fmt"
)

// EventEnvelopeV2 is the v2 envelope schema for the unified event stream.
// Core tracing fields are top-level; source-specific data is in Payload.
//
// Version: 2 for native v2 envelopes. ParseEnvelopeV2JSON sets Version=1
// when parsing legacy v1 envelopes that lack the field.
//
// PayloadType identifies which typed struct is in Payload. Callers use this
// (not Event.Source) to select the correct ParsePayload[T] instantiation.
// Values: "phase", "dispatch", "coordination", "legacy" (v1 fallback).
type EventEnvelopeV2 struct {
	Version        int             `json:"version,omitempty"      jsonschema:"enum=1,enum=2"`
	TraceID        string          `json:"trace_id,omitempty"`
	SpanID         string          `json:"span_id,omitempty"`
	ParentSpanID   string          `json:"parent_span_id,omitempty"`
	CallerIdentity string          `json:"caller_identity,omitempty"`
	PayloadType    string          `json:"payload_type,omitempty" jsonschema:"enum=phase,enum=dispatch,enum=coordination,enum=legacy"`
	Payload        json.RawMessage `json:"payload,omitempty"`
}

// PhasePayload carries phase-specific envelope data.
// Note: FromPhase/ToPhase are NOT included here — they duplicate Event.FromState/ToState.
// Only source-specific data that has no other representation belongs in the payload.
type PhasePayload struct {
	CapabilityScope string `json:"capability_scope,omitempty"` // "run:{id}"
}

// DispatchPayload carries dispatch-specific envelope data.
// Note: FromStatus/ToStatus are NOT included — they duplicate Event.FromState/ToState.
type DispatchPayload struct {
	DispatchID       string `json:"dispatch_id,omitempty"`
	CapabilityScope  string `json:"capability_scope,omitempty"` // "run:{id}" or "dispatch:{id}"
	RequestedSandbox string `json:"requested_sandbox,omitempty"`
	EffectiveSandbox string `json:"effective_sandbox,omitempty"`
}

// CoordinationPayload carries coordination-specific envelope data.
type CoordinationPayload struct {
	LockID string `json:"lock_id,omitempty"`
	Owner  string `json:"owner,omitempty"`
	Scope  string `json:"scope,omitempty"`
}

// MarshalPayload marshals a typed payload into json.RawMessage.
func MarshalPayload(v any) (json.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	return json.Marshal(v)
}

// MarshalEnvelopeV2JSON serializes a v2 envelope for database storage.
// Returns nil when the envelope is nil. Does NOT mutate the input struct.
func MarshalEnvelopeV2JSON(e *EventEnvelopeV2) (*string, error) {
	if e == nil {
		return nil, nil
	}
	// Stack copy to avoid mutating caller's struct (C1 fix).
	out := *e
	if out.Version == 0 {
		out.Version = 2
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	s := string(b)
	return &s, nil
}

// ParseEnvelopeV2JSON decodes envelope JSON, handling both v1 and v2 formats.
//
// For v2 envelopes (version >= 2): decodes directly into EventEnvelopeV2.
// For v1 envelopes (no version field): maps core tracing fields to v2 with
// Version=1 and stores the full v1 JSON in Payload with PayloadType="legacy".
// This preserves all v1 fields (including RequestedSandbox, EffectiveSandbox,
// InputArtifactRefs, etc.) — use ParsePayload[EventEnvelope] to recover them.
//
// Returns nil for empty input.
func ParseEnvelopeV2JSON(raw string) (*EventEnvelopeV2, error) {
	if raw == "" {
		return nil, nil
	}

	// Probe version field first.
	var probe struct {
		Version *int `json:"version"`
	}
	if err := json.Unmarshal([]byte(raw), &probe); err != nil {
		return nil, fmt.Errorf("parse envelope version: %w", err)
	}

	if probe.Version != nil && *probe.Version >= 2 {
		// Native v2.
		var e EventEnvelopeV2
		if err := json.Unmarshal([]byte(raw), &e); err != nil {
			return nil, fmt.Errorf("parse envelope v2: %w", err)
		}
		return &e, nil
	}

	// v1 fallback: parse as v1 to extract core fields, store full JSON as payload.
	v1, err := ParseEnvelopeJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("parse envelope v1: %w", err)
	}
	if v1 == nil {
		return nil, nil
	}

	return &EventEnvelopeV2{
		Version:        1,
		TraceID:        v1.TraceID,
		SpanID:         v1.SpanID,
		ParentSpanID:   v1.ParentSpanID,
		CallerIdentity: v1.CallerIdentity,
		PayloadType:    "legacy",
		Payload:        json.RawMessage(raw), // Preserve ALL v1 fields (C2 fix).
	}, nil
}

// ParsePayload unmarshals the envelope payload into a typed struct.
// The caller must select T based on PayloadType:
//
//	"phase"        → ParsePayload[PhasePayload]
//	"dispatch"     → ParsePayload[DispatchPayload]
//	"coordination" → ParsePayload[CoordinationPayload]
//	"legacy"       → ParsePayload[EventEnvelope] (full v1 envelope)
//
// Returns (nil, nil) when payload is empty or envelope is nil.
func ParsePayload[T any](e *EventEnvelopeV2) (*T, error) {
	if e == nil || len(e.Payload) == 0 {
		return nil, nil
	}
	var v T
	if err := json.Unmarshal(e.Payload, &v); err != nil {
		return nil, fmt.Errorf("parse %T payload: %w", v, err)
	}
	return &v, nil
}
