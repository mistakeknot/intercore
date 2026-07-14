package main

import (
	"context"
	"testing"

	"github.com/mistakeknot/intercore/internal/event"
)

func TestCmdEventsRecordInterspectIsIdempotentWithExplicitKey(t *testing.T) {
	setupCommandMetadataDB(t)
	ctx := context.Background()
	args := []string{
		"record",
		"--source=interspect",
		"--type=remontoire.stage",
		`--payload={"agent_name":"remontoire","context":"{\"stage\":\"reviewing\"}"}`,
		"--run=run-1",
		"--idempotency-key=cycle-1:reviewing",
	}

	if rc := cmdEvents(ctx, args); rc != 0 {
		t.Fatalf("first record rc = %d", rc)
	}
	if rc := cmdEvents(ctx, args); rc != 0 {
		t.Fatalf("second record rc = %d", rc)
	}

	d, err := openDB()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	events, err := event.NewStore(d.SqlDB()).ListInterspectEvents(ctx, "remontoire", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %#v", events)
	}
	if events[0].SessionID != "cycle-1:reviewing" {
		t.Fatalf("session_id = %q", events[0].SessionID)
	}
}

func TestCmdEventsRecordAgencyIsIdempotentWithExplicitKey(t *testing.T) {
	setupCommandMetadataDB(t)
	ctx := context.Background()
	args := []string{
		"record",
		"--source=agency",
		"--type=agency.stage",
		`--payload={"agency_name":"remontoire","cycle_id":"cycle-1","stage":"reviewing","context":{"candidate_count":3}}`,
		"--run=run-1",
		"--idempotency-key=cycle-1:reviewing",
	}

	if rc := cmdEvents(ctx, args); rc != 0 {
		t.Fatalf("first record rc = %d", rc)
	}
	if rc := cmdEvents(ctx, args); rc != 0 {
		t.Fatalf("second record rc = %d", rc)
	}

	d, err := openDB()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	events, err := event.NewStore(d.SqlDB()).ListAgencyEvents(ctx, "remontoire", "run-1", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %#v", events)
	}
	if events[0].CycleID != "cycle-1" || events[0].Stage != "reviewing" || events[0].ContextJSON != `{"candidate_count":3}` {
		t.Fatalf("event = %#v", events[0])
	}
}
