package event

import (
	"encoding/json"
	"testing"
)

// v1 fixtures — actual JSON produced by existing envelope constructors.
const v1PhaseEnvelope = `{"policy_version":"phase-machine/v1","caller_identity":"phase.store","capability_scope":"run:run-abc","trace_id":"run-abc","span_id":"phase:advance:1234","parent_span_id":"phase-state:brainstorm","input_artifact_refs":["phase:brainstorm"],"output_artifact_refs":["phase:planned"]}`

const v1DispatchEnvelope = `{"policy_version":"dispatch-lifecycle/v2","caller_identity":"dispatch.store","capability_scope":"run:run-xyz","trace_id":"run-xyz","span_id":"dispatch:disp-1:5678","parent_span_id":"","input_artifact_refs":["spawned"],"output_artifact_refs":["running"],"requested_sandbox":"{\"mode\":\"workspace-write\"}","effective_sandbox":"{\"mode\":\"workspace-read\"}"}`

const v1CoordinationEnvelope = `{"policy_version":"coordination/v1","caller_identity":"coordination.store","capability_scope":"scope:project","trace_id":"lock-abc","span_id":"coordination:lock_acquired:9999","parent_span_id":"","input_artifact_refs":["*.go"],"output_artifact_refs":["lock-abc"]}`

func TestEnvelopeV2_RoundTrip_PhasePayload(t *testing.T) {
	payload, err := MarshalPayload(PhasePayload{
		CapabilityScope: "run:test-123",
	})
	if err != nil {
		t.Fatalf("MarshalPayload: %v", err)
	}

	env := &EventEnvelopeV2{
		Version:        2,
		TraceID:        "run-123",
		SpanID:         "phase:advance:1",
		ParentSpanID:   "phase-state:brainstorm",
		CallerIdentity: "phase.store",
		PayloadType:    "phase",
		Payload:        payload,
	}

	s, err := MarshalEnvelopeV2JSON(env)
	if err != nil {
		t.Fatalf("MarshalEnvelopeV2JSON: %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil result")
	}

	parsed, err := ParseEnvelopeV2JSON(*s)
	if err != nil {
		t.Fatalf("ParseEnvelopeV2JSON: %v", err)
	}

	if parsed.Version != 2 {
		t.Errorf("Version = %d, want 2", parsed.Version)
	}
	if parsed.TraceID != "run-123" {
		t.Errorf("TraceID = %q, want %q", parsed.TraceID, "run-123")
	}
	if parsed.PayloadType != "phase" {
		t.Errorf("PayloadType = %q, want %q", parsed.PayloadType, "phase")
	}

	pp, err := ParsePayload[PhasePayload](parsed)
	if err != nil {
		t.Fatalf("ParsePayload[PhasePayload]: %v", err)
	}
	if pp == nil {
		t.Fatal("expected non-nil PhasePayload")
	}
	if pp.CapabilityScope != "run:test-123" {
		t.Errorf("CapabilityScope = %q, want %q", pp.CapabilityScope, "run:test-123")
	}
}

func TestEnvelopeV2_RoundTrip_DispatchPayload(t *testing.T) {
	payload, err := MarshalPayload(DispatchPayload{
		DispatchID:       "disp-abc",
		CapabilityScope:  "run:run-xyz",
		RequestedSandbox: `{"mode":"workspace-write"}`,
		EffectiveSandbox: `{"mode":"workspace-read"}`,
	})
	if err != nil {
		t.Fatalf("MarshalPayload: %v", err)
	}

	env := &EventEnvelopeV2{
		Version:        2,
		TraceID:        "run-xyz",
		SpanID:         "dispatch:disp-abc:1",
		CallerIdentity: "dispatch.store",
		PayloadType:    "dispatch",
		Payload:        payload,
	}

	s, err := MarshalEnvelopeV2JSON(env)
	if err != nil {
		t.Fatalf("MarshalEnvelopeV2JSON: %v", err)
	}

	parsed, err := ParseEnvelopeV2JSON(*s)
	if err != nil {
		t.Fatalf("ParseEnvelopeV2JSON: %v", err)
	}

	dp, err := ParsePayload[DispatchPayload](parsed)
	if err != nil {
		t.Fatalf("ParsePayload[DispatchPayload]: %v", err)
	}
	if dp.RequestedSandbox != `{"mode":"workspace-write"}` {
		t.Errorf("RequestedSandbox = %q", dp.RequestedSandbox)
	}
	if dp.EffectiveSandbox != `{"mode":"workspace-read"}` {
		t.Errorf("EffectiveSandbox = %q", dp.EffectiveSandbox)
	}
}

func TestEnvelopeV2_RoundTrip_CoordinationPayload(t *testing.T) {
	payload, err := MarshalPayload(CoordinationPayload{
		LockID: "lock-abc",
		Owner:  "agent-a",
		Scope:  "project",
	})
	if err != nil {
		t.Fatalf("MarshalPayload: %v", err)
	}

	env := &EventEnvelopeV2{
		Version:        2,
		TraceID:        "lock-abc",
		SpanID:         "coordination:lock_acquired:1",
		CallerIdentity: "coordination.store",
		PayloadType:    "coordination",
		Payload:        payload,
	}

	s, err := MarshalEnvelopeV2JSON(env)
	if err != nil {
		t.Fatalf("MarshalEnvelopeV2JSON: %v", err)
	}

	parsed, err := ParseEnvelopeV2JSON(*s)
	if err != nil {
		t.Fatalf("ParseEnvelopeV2JSON: %v", err)
	}

	cp, err := ParsePayload[CoordinationPayload](parsed)
	if err != nil {
		t.Fatalf("ParsePayload[CoordinationPayload]: %v", err)
	}
	if cp.LockID != "lock-abc" {
		t.Errorf("LockID = %q, want %q", cp.LockID, "lock-abc")
	}
	if cp.Owner != "agent-a" {
		t.Errorf("Owner = %q, want %q", cp.Owner, "agent-a")
	}
}

func TestEnvelopeV2_RoundTrip_NilPayload(t *testing.T) {
	env := &EventEnvelopeV2{
		Version:        2,
		TraceID:        "run-nopayload",
		CallerIdentity: "test",
	}

	s, err := MarshalEnvelopeV2JSON(env)
	if err != nil {
		t.Fatalf("MarshalEnvelopeV2JSON: %v", err)
	}

	parsed, err := ParseEnvelopeV2JSON(*s)
	if err != nil {
		t.Fatalf("ParseEnvelopeV2JSON: %v", err)
	}
	if parsed.Version != 2 {
		t.Errorf("Version = %d, want 2", parsed.Version)
	}
	if parsed.TraceID != "run-nopayload" {
		t.Errorf("TraceID = %q", parsed.TraceID)
	}

	pp, err := ParsePayload[PhasePayload](parsed)
	if err != nil {
		t.Fatalf("ParsePayload on nil: %v", err)
	}
	if pp != nil {
		t.Errorf("expected nil payload, got %+v", pp)
	}
}

func TestEnvelopeV2_V1Fallback_Phase(t *testing.T) {
	parsed, err := ParseEnvelopeV2JSON(v1PhaseEnvelope)
	if err != nil {
		t.Fatalf("ParseEnvelopeV2JSON(v1 phase): %v", err)
	}

	if parsed.Version != 1 {
		t.Errorf("Version = %d, want 1", parsed.Version)
	}
	if parsed.TraceID != "run-abc" {
		t.Errorf("TraceID = %q, want %q", parsed.TraceID, "run-abc")
	}
	if parsed.CallerIdentity != "phase.store" {
		t.Errorf("CallerIdentity = %q, want %q", parsed.CallerIdentity, "phase.store")
	}
	if parsed.PayloadType != "legacy" {
		t.Errorf("PayloadType = %q, want %q", parsed.PayloadType, "legacy")
	}

	// Recover full v1 data from Payload — no field loss (C2 fix).
	v1, err := ParsePayload[EventEnvelope](parsed)
	if err != nil {
		t.Fatalf("ParsePayload[EventEnvelope]: %v", err)
	}
	if v1 == nil {
		t.Fatal("expected non-nil v1 envelope from legacy payload")
	}
	if v1.PolicyVersion != "phase-machine/v1" {
		t.Errorf("PolicyVersion = %q, want %q", v1.PolicyVersion, "phase-machine/v1")
	}
	if v1.CapabilityScope != "run:run-abc" {
		t.Errorf("CapabilityScope = %q, want %q", v1.CapabilityScope, "run:run-abc")
	}
	if len(v1.InputArtifactRefs) != 1 || v1.InputArtifactRefs[0] != "phase:brainstorm" {
		t.Errorf("InputArtifactRefs = %v, want [phase:brainstorm]", v1.InputArtifactRefs)
	}
	if len(v1.OutputArtifactRefs) != 1 || v1.OutputArtifactRefs[0] != "phase:planned" {
		t.Errorf("OutputArtifactRefs = %v, want [phase:planned]", v1.OutputArtifactRefs)
	}
}

func TestEnvelopeV2_V1Fallback_Dispatch(t *testing.T) {
	parsed, err := ParseEnvelopeV2JSON(v1DispatchEnvelope)
	if err != nil {
		t.Fatalf("ParseEnvelopeV2JSON(v1 dispatch): %v", err)
	}

	if parsed.Version != 1 {
		t.Errorf("Version = %d, want 1", parsed.Version)
	}
	if parsed.TraceID != "run-xyz" {
		t.Errorf("TraceID = %q, want %q", parsed.TraceID, "run-xyz")
	}
	if parsed.PayloadType != "legacy" {
		t.Errorf("PayloadType = %q, want %q", parsed.PayloadType, "legacy")
	}

	// Recover sandbox fields from legacy payload — the C2 critical fix.
	v1, err := ParsePayload[EventEnvelope](parsed)
	if err != nil {
		t.Fatalf("ParsePayload[EventEnvelope]: %v", err)
	}
	if v1 == nil {
		t.Fatal("expected non-nil v1 envelope")
	}
	if v1.RequestedSandbox != `{"mode":"workspace-write"}` {
		t.Errorf("RequestedSandbox = %q", v1.RequestedSandbox)
	}
	if v1.EffectiveSandbox != `{"mode":"workspace-read"}` {
		t.Errorf("EffectiveSandbox = %q", v1.EffectiveSandbox)
	}
}

func TestEnvelopeV2_V1Fallback_Coordination(t *testing.T) {
	parsed, err := ParseEnvelopeV2JSON(v1CoordinationEnvelope)
	if err != nil {
		t.Fatalf("ParseEnvelopeV2JSON(v1 coordination): %v", err)
	}

	if parsed.Version != 1 {
		t.Errorf("Version = %d, want 1", parsed.Version)
	}
	if parsed.CallerIdentity != "coordination.store" {
		t.Errorf("CallerIdentity = %q", parsed.CallerIdentity)
	}

	// Recover coordination-specific fields via legacy payload.
	v1, err := ParsePayload[EventEnvelope](parsed)
	if err != nil {
		t.Fatalf("ParsePayload: %v", err)
	}
	if v1.CapabilityScope != "scope:project" {
		t.Errorf("CapabilityScope = %q, want %q", v1.CapabilityScope, "scope:project")
	}
}

func TestEnvelopeV2_EmptyInput(t *testing.T) {
	parsed, err := ParseEnvelopeV2JSON("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if parsed != nil {
		t.Errorf("expected nil for empty input, got %+v", parsed)
	}
}

func TestMarshalPayload_Nil(t *testing.T) {
	raw, err := MarshalPayload(nil)
	if err != nil {
		t.Fatalf("MarshalPayload(nil): %v", err)
	}
	if raw != nil {
		t.Errorf("expected nil, got %s", raw)
	}
}

func TestMarshalEnvelopeV2JSON_DoesNotMutateInput(t *testing.T) {
	env := &EventEnvelopeV2{
		TraceID: "test",
		// Version deliberately left at 0.
	}

	_, err := MarshalEnvelopeV2JSON(env)
	if err != nil {
		t.Fatalf("MarshalEnvelopeV2JSON: %v", err)
	}

	// C1 fix: Version should NOT have been mutated.
	if env.Version != 0 {
		t.Errorf("MarshalEnvelopeV2JSON mutated input: Version = %d, want 0", env.Version)
	}
}

func TestMarshalEnvelopeV2JSON_Nil(t *testing.T) {
	s, err := MarshalEnvelopeV2JSON(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s != nil {
		t.Errorf("expected nil for nil input, got %q", *s)
	}
}

func TestMarshalEnvelopeV2JSON_DefaultVersion(t *testing.T) {
	env := &EventEnvelopeV2{TraceID: "test"}
	s, err := MarshalEnvelopeV2JSON(env)
	if err != nil {
		t.Fatal(err)
	}

	// Should output version:2 even though input was 0.
	var probe struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal([]byte(*s), &probe); err != nil {
		t.Fatal(err)
	}
	if probe.Version != 2 {
		t.Errorf("serialized version = %d, want 2", probe.Version)
	}
}

func TestParsePayload_WrongType_SilentZero(t *testing.T) {
	// Marshaling a DispatchPayload then parsing as PhasePayload should
	// succeed but yield zero-valued fields (Go json behavior).
	// PayloadType discriminator prevents this in practice.
	raw, _ := MarshalPayload(DispatchPayload{
		DispatchID:       "disp-1",
		RequestedSandbox: "sandbox",
	})
	env := &EventEnvelopeV2{
		Version:     2,
		PayloadType: "dispatch", // correct type is dispatch
		Payload:     raw,
	}

	// Intentionally parse as wrong type.
	pp, err := ParsePayload[PhasePayload](env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pp == nil {
		t.Fatal("expected non-nil (json ignores unknown fields)")
	}
	// CapabilityScope should be empty because DispatchPayload has a matching field.
	// This test documents the behavior — PayloadType is the guard.
	t.Logf("wrong-type parse: %+v (PayloadType=%q prevents this in practice)", pp, env.PayloadType)
}
