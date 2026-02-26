package phase

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

type phaseEventEnvelope struct {
	PolicyVersion      string   `json:"policy_version,omitempty"`
	CallerIdentity     string   `json:"caller_identity,omitempty"`
	CapabilityScope    string   `json:"capability_scope,omitempty"`
	TraceID            string   `json:"trace_id,omitempty"`
	SpanID             string   `json:"span_id,omitempty"`
	ParentSpanID       string   `json:"parent_span_id,omitempty"`
	InputArtifactRefs  []string `json:"input_artifact_refs,omitempty"`
	OutputArtifactRefs []string `json:"output_artifact_refs,omitempty"`
}

func defaultPhaseEnvelopeJSON(runID, eventType, fromPhase, toPhase string) *string {
	// Read propagated trace context from environment
	envTraceID := os.Getenv("IC_TRACE_ID")
	envSpanID := os.Getenv("IC_SPAN_ID")

	traceID := envTraceID
	if traceID == "" {
		traceID = runID
	}

	spanID := envSpanID
	if spanID == "" {
		spanID = fmt.Sprintf("phase:%s:%d", eventType, time.Now().UnixNano())
	}

	capabilityScope := ""
	if runID != "" {
		capabilityScope = "run:" + runID
	}

	envelope := phaseEventEnvelope{
		PolicyVersion:      "phase-machine/v1",
		CallerIdentity:     "phase.store",
		CapabilityScope:    capabilityScope,
		TraceID:            traceID,
		SpanID:             spanID,
		ParentSpanID:       fmt.Sprintf("phase-state:%s", fromPhase),
		InputArtifactRefs:  phaseRef(fromPhase),
		OutputArtifactRefs: phaseRef(toPhase),
	}

	b, err := json.Marshal(envelope)
	if err != nil {
		return nil
	}
	s := string(b)
	return &s
}

func phaseRef(phase string) []string {
	if phase == "" {
		return nil
	}
	return []string{"phase:" + phase}
}
