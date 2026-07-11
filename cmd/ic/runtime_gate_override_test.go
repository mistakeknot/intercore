package main

import (
	"context"
	"strings"
	"testing"

	"github.com/mistakeknot/intercore/internal/phase"
)

func TestCmdGateOverrideCannotBypassRuntimeTerminalGate(t *testing.T) {
	setupCommandMetadataDB(t)
	ctx := context.Background()
	d, err := openDB()
	if err != nil {
		t.Fatal(err)
	}
	store := phase.New(d.SqlDB())
	metadata := `{"close_gate":{"requirements":["runtime-evidence/v1"],"bead_id":"sylveste-6h7x"}}`
	runID, err := store.Create(ctx, &phase.Run{
		ProjectDir: "/tmp", Goal: "override", Complexity: 3, AutoAdvance: true,
		Phases: []string{phase.PhaseReflect, phase.PhaseDone}, Metadata: &metadata,
	})
	if err != nil {
		d.Close()
		t.Fatal(err)
	}
	d.Close()

	if rc := cmdGateOverride(ctx, []string{runID, "--reason=force", "--justified"}); rc != 1 {
		t.Fatalf("cmdGateOverride rc = %d, want 1", rc)
	}
	d, err = openDB()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	store = phase.New(d.SqlDB())
	run, err := store.Get(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Phase != phase.PhaseReflect || run.Status != phase.StatusActive {
		t.Fatalf("override advanced runtime-gated run: phase=%s status=%s", run.Phase, run.Status)
	}
	events, err := store.Events(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].EventType != phase.EventBlock || events[0].Reason == nil || !strings.Contains(*events[0].Reason, phase.CheckRuntimeEvidence) {
		t.Fatalf("override block event = %+v", events)
	}
}
