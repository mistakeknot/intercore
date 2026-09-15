package event

import (
	"context"
	"reflect"
	"testing"

	"github.com/mistakeknot/intercore/internal/dispatch"
)

// The dispatch package builds its terminal event's envelope itself, because it
// cannot import this package. It must match what this store builds for the same
// transition; only the span id, which carries a timestamp, may differ.
func TestTerminalEventEnvelopeMatchesDispatchEnvelope(t *testing.T) {
	store, d := setupTestStore(t)
	ctx := context.Background()
	ds := dispatch.New(d.SqlDB(), nil)

	runID := "run-envelope"
	spec := `{"mode":"workspace-write"}`
	id, err := ds.Create(ctx, &dispatch.Dispatch{AgentType: "codex", ProjectDir: t.TempDir(), ScopeID: &runID, SandboxSpec: &spec})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := ds.UpdateStatus(ctx, id, dispatch.StatusRunning, nil); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	if _, err := ds.Terminalize(ctx, id, dispatch.Terminal{
		Status:   dispatch.StatusCompleted,
		Source:   dispatch.TerminalSourceCollect,
		Evidence: dispatch.EvidenceNotApplicable,
		Fields:   dispatch.UpdateFields{"sandbox_effective": `{"mode":"read-only"}`},
	}); err != nil {
		t.Fatalf("Terminalize: %v", err)
	}

	var raw string
	if err := d.SqlDB().QueryRowContext(ctx,
		"SELECT envelope_json FROM dispatch_events WHERE dispatch_id = ? AND event_type = 'terminal'", id).Scan(&raw); err != nil {
		t.Fatalf("terminal event: %v", err)
	}
	got, err := ParseEnvelopeJSON(raw)
	if err != nil {
		t.Fatalf("parse envelope %s: %v", raw, err)
	}
	want := store.defaultDispatchEnvelope(ctx, id, runID, dispatch.StatusRunning, dispatch.StatusCompleted)
	got.SpanID, want.SpanID = "", ""
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("terminal event envelope = %+v, want %+v", got, want)
	}
}
