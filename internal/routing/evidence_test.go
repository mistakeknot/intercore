package routing

import (
	"context"
	"encoding/json"
	"testing"
)

// TestEscalationObservableInEvidence is the executable form of the DoD's
// "escalation observable in evidence" clause: an escalation step is recorded
// and then FOUND by querying the decision store, with the model transition and
// the structured event intact.
func TestEscalationObservableInEvidence(t *testing.T) {
	store := testDecisionStore(t)
	ctx := context.Background()

	// A sonnet->opus escalation after a verdict failure.
	id, err := store.RecordEscalation(ctx, EscalationEvidence{
		ChainKey:   "chain-abc",
		FromModel:  "sonnet",
		ToModel:    "opus",
		StrikeMode: "verdict-fail",
		Detail:     "second strike at sonnet",
		Agent:      "executor",
		ProjectDir: "/proj",
	})
	if err != nil {
		t.Fatalf("RecordEscalation: %v", err)
	}
	if id <= 0 {
		t.Fatalf("RecordEscalation id = %d, want > 0", id)
	}

	// It must be OBSERVABLE: queryable from the store, model transition intact.
	decisions, err := store.List(ctx, ListDecisionOpts{ProjectDir: "/proj"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var found *Decision
	for i := range decisions {
		if decisions[i].ID == id {
			found = &decisions[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("escalation decision id=%d not found in evidence", id)
	}
	if found.RuleMatched != "escalation" {
		t.Errorf("rule = %q, want escalation (so it is filterable)", found.RuleMatched)
	}
	if found.SelectedModel != "opus" {
		t.Errorf("selected model = %q, want opus (the escalated-to rung)", found.SelectedModel)
	}

	// The structured event must round-trip through ContextJSON.
	if found.ContextJSON == nil {
		t.Fatal("ContextJSON is nil; escalation event payload lost")
	}
	var ec escalationContext
	if err := json.Unmarshal([]byte(*found.ContextJSON), &ec); err != nil {
		t.Fatalf("unmarshal escalation context: %v", err)
	}
	if ec.Event != "escalation" || ec.ChainKey != "chain-abc" || ec.StrikeMode != "verdict-fail" {
		t.Errorf("escalation context = %+v, want event=escalation chain=chain-abc mode=verdict-fail", ec)
	}
	if ec.Exhausted {
		t.Error("Exhausted = true, want false for a forward escalation")
	}
}

// TestExhaustedEscalationObservable covers the ladder-exhausted terminal event.
func TestExhaustedEscalationObservable(t *testing.T) {
	store := testDecisionStore(t)
	ctx := context.Background()

	id, err := store.RecordEscalation(ctx, EscalationEvidence{
		ChainKey:   "chain-xyz",
		FromModel:  "opus",
		ToModel:    "", // nowhere higher (frontier window closed)
		StrikeMode: "timeout",
		Exhausted:  true,
		Agent:      "executor",
		ProjectDir: "/proj",
	})
	if err != nil {
		t.Fatalf("RecordEscalation: %v", err)
	}

	decisions, err := store.List(ctx, ListDecisionOpts{ProjectDir: "/proj"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var found *Decision
	for i := range decisions {
		if decisions[i].ID == id {
			found = &decisions[i]
		}
	}
	if found == nil {
		t.Fatal("exhausted escalation not observable")
	}
	var ec escalationContext
	if err := json.Unmarshal([]byte(*found.ContextJSON), &ec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ec.Event != "escalation_exhausted" || !ec.Exhausted {
		t.Errorf("event = %q exhausted = %v, want escalation_exhausted/true", ec.Event, ec.Exhausted)
	}
}
