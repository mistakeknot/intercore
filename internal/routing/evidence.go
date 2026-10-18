package routing

import (
	"context"
	"encoding/json"
)

// This file makes escalation and gate outcomes OBSERVABLE IN EVIDENCE (the
// DoD's second clause) by recording them onto the EXISTING routing-decision
// store rather than a parallel evidence table. The plan (f-014) explicitly
// warns against a second witness store next to the one that already exists;
// this reuses DecisionStore so escalations surface via `ic route list`.
//
// This is the emission side of the Phase 5 evidence loop. The full evidence
// event schema (§4: gate_executed, escalation_expired, etc. as first-class
// event types with a decision-witness ref) is still pending the §4 interview;
// what lands here is the escalation/gate outcome as an observable decision
// record, which is sufficient for the DoD's "escalation observable" clause.

// EscalationEvidence captures one escalation step for the evidence trail.
type EscalationEvidence struct {
	ChainKey   string // the escalation chain this step belongs to
	FromModel  string // model that failed
	ToModel    string // model escalated to ("" if exhausted)
	StrikeMode string // failure mode that triggered the strike
	Detail     string // free-text lesson/detail
	Exhausted  bool   // true if the ladder was exhausted (no ToModel)
	Agent      string // the agent/role being escalated
	ProjectDir string
	RunID      string
	SessionID  string
}

// escalationContext is the structured payload stored in the decision's
// ContextJSON so `ic route list` / evidence consumers can reconstruct the
// escalation without a schema change to the decisions table.
type escalationContext struct {
	Event      string `json:"event"` // "escalation" | "escalation_exhausted"
	ChainKey   string `json:"chain_key"`
	StrikeMode string `json:"strike_mode"`
	Detail     string `json:"detail,omitempty"`
	Exhausted  bool   `json:"exhausted"`
}

// RecordEscalation writes an escalation step to the decision store as an
// observable evidence record. RuleMatched is "escalation" so these are
// filterable; FloorFrom/FloorTo carry the model transition; the structured
// event lives in ContextJSON. Returns the decision id.
func (s *DecisionStore) RecordEscalation(ctx context.Context, ev EscalationEvidence) (int64, error) {
	event := "escalation"
	selected := ev.ToModel
	if ev.Exhausted {
		event = "escalation_exhausted"
		selected = ev.FromModel // no forward model; record the last one tried
	}

	payload, err := json.Marshal(escalationContext{
		Event:      event,
		ChainKey:   ev.ChainKey,
		StrikeMode: ev.StrikeMode,
		Detail:     ev.Detail,
		Exhausted:  ev.Exhausted,
	})
	if err != nil {
		return 0, err
	}

	return s.Record(ctx, RecordDecisionOpts{
		ProjectDir:    ev.ProjectDir,
		RunID:         ev.RunID,
		SessionID:     ev.SessionID,
		Agent:         ev.Agent,
		Category:      "escalation",
		SelectedModel: selected,
		RuleMatched:   "escalation",
		FloorFrom:     ev.FromModel,
		FloorTo:       ev.ToModel,
		ContextJSON:   string(payload),
	})
}

// GateEvidence captures whether a required verification gate actually ran
// before an executor's output was accepted (fd-architecture F6 / finding
// f-006). A named-but-unrun gate is the silently-disabled reward-hacking
// control the Sol research motivated; recording gate_executed vs gate_skipped
// makes a skipped gate detectable after the fact.
type GateEvidence struct {
	Gate       string // the named gate, e.g. "behavioral-verify"
	Model      string // the model whose output the gate guarded
	Executed   bool   // true = gate ran; false = gate was skipped
	Agent      string
	ProjectDir string
	RunID      string
	SessionID  string
}

type gateContext struct {
	Event string `json:"event"` // "gate_executed" | "gate_skipped"
	Gate  string `json:"gate"`
}

// RecordGate writes a gate-execution outcome to the decision store as an
// observable evidence record. RuleMatched is "gate" so gate events are
// filterable; the executed/skipped event lives in ContextJSON. A gate_skipped
// record is the audit trail proving a required gate did NOT run.
func (s *DecisionStore) RecordGate(ctx context.Context, ev GateEvidence) (int64, error) {
	event := "gate_skipped"
	if ev.Executed {
		event = "gate_executed"
	}
	payload, err := json.Marshal(gateContext{Event: event, Gate: ev.Gate})
	if err != nil {
		return 0, err
	}
	return s.Record(ctx, RecordDecisionOpts{
		ProjectDir:    ev.ProjectDir,
		RunID:         ev.RunID,
		SessionID:     ev.SessionID,
		Agent:         ev.Agent,
		Category:      "gate",
		SelectedModel: ev.Model,
		RuleMatched:   "gate",
		ContextJSON:   string(payload),
	})
}
