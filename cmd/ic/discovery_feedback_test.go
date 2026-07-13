package main

import (
	"context"
	"testing"

	"github.com/mistakeknot/intercore/internal/discovery"
)

func TestCmdDiscoveryFeedbackUsesExplicitIdempotencyKey(t *testing.T) {
	setupCommandMetadataDB(t)
	ctx := context.Background()
	d, err := openDB()
	if err != nil {
		t.Fatal(err)
	}
	id, err := discovery.NewStore(d.SqlDB()).Submit(ctx, "test", "feedback-once", "Fixture", "", "", "{}", nil, 0.5)
	if err != nil {
		d.Close()
		t.Fatal(err)
	}
	d.Close()

	args := []string{id, "--signal=boost", "--actor=remontoire", "--idempotency-key=remontoire:cycle-1:" + id}
	if rc := cmdDiscoveryFeedback(ctx, args); rc != 0 {
		t.Fatalf("first feedback rc = %d", rc)
	}
	if rc := cmdDiscoveryFeedback(ctx, args); rc != 0 {
		t.Fatalf("second feedback rc = %d", rc)
	}

	d, err = openDB()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var signals, events int
	if err := d.SqlDB().QueryRow(`SELECT COUNT(*) FROM feedback_signals WHERE discovery_id = ?`, id).Scan(&signals); err != nil {
		t.Fatal(err)
	}
	if err := d.SqlDB().QueryRow(`SELECT COUNT(*) FROM discovery_events WHERE discovery_id = ? AND event_type = ?`, id, discovery.EventFeedback).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if signals != 1 || events != 1 {
		t.Fatalf("signals=%d events=%d", signals, events)
	}
}
