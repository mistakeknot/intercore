package event

import (
	"encoding/json"
)

// EventEnvelope carries optional provenance data for causal audit and replay.
type EventEnvelope struct {
	PolicyVersion      string   `json:"policy_version,omitempty"`
	CallerIdentity     string   `json:"caller_identity,omitempty"`
	CapabilityScope    string   `json:"capability_scope,omitempty"`
	TraceID            string   `json:"trace_id,omitempty"`
	SpanID             string   `json:"span_id,omitempty"`
	ParentSpanID       string   `json:"parent_span_id,omitempty"`
	InputArtifactRefs  []string `json:"input_artifact_refs,omitempty"`
	OutputArtifactRefs []string `json:"output_artifact_refs,omitempty"`
	RequestedSandbox   string   `json:"requested_sandbox,omitempty"`
	EffectiveSandbox   string   `json:"effective_sandbox,omitempty"`
}

// IsZero reports whether the envelope has no meaningful fields set.
func (e *EventEnvelope) IsZero() bool {
	if e == nil {
		return true
	}
	return e.PolicyVersion == "" &&
		e.CallerIdentity == "" &&
		e.CapabilityScope == "" &&
		e.TraceID == "" &&
		e.SpanID == "" &&
		e.ParentSpanID == "" &&
		len(e.InputArtifactRefs) == 0 &&
		len(e.OutputArtifactRefs) == 0 &&
		e.RequestedSandbox == "" &&
		e.EffectiveSandbox == ""
}

// MarshalEnvelopeJSON converts an envelope to JSON for database storage.
// Returns nil when the envelope is nil/empty.
func MarshalEnvelopeJSON(e *EventEnvelope) (*string, error) {
	if e == nil || e.IsZero() {
		return nil, nil
	}
	b, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	s := string(b)
	return &s, nil
}

// ParseEnvelopeJSON decodes envelope JSON from storage.
func ParseEnvelopeJSON(raw string) (*EventEnvelope, error) {
	if raw == "" {
		return nil, nil
	}
	var e EventEnvelope
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		return nil, err
	}
	if e.IsZero() {
		return nil, nil
	}
	return &e, nil
}
