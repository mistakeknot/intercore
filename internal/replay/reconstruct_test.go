package replay

import (
	"reflect"
	"testing"
	"time"

	"github.com/mistakeknot/intercore/internal/event"
)

func TestBuildTimeline_Deterministic(t *testing.T) {
	now := time.Unix(1700000000, 0)
	events := []event.Event{
		{
			ID:        1,
			Source:    event.SourcePhase,
			Type:      "advance",
			FromState: "brainstorm",
			ToState:   "planned",
			Timestamp: now,
		},
		{
			ID:        2,
			Source:    event.SourceDispatch,
			Type:      "status_change",
			FromState: "spawned",
			ToState:   "running",
			Timestamp: now.Add(time.Second),
		},
	}
	eventID := int64(2)
	inputs := []*Input{
		{
			ID:          10,
			RunID:       "run1",
			Kind:        "external",
			EventSource: event.SourceDispatch,
			EventID:     &eventID,
		},
	}

	got1 := BuildTimeline(events, inputs)
	got2 := BuildTimeline(events, inputs)
	if !reflect.DeepEqual(got1, got2) {
		t.Fatalf("BuildTimeline not deterministic:\nfirst=%#v\nsecond=%#v", got1, got2)
	}
}

func TestBuildTimeline_CrashRetryPath(t *testing.T) {
	now := time.Unix(1700000100, 0)
	events := []event.Event{
		{
			ID:        11,
			Source:    event.SourceDispatch,
			Type:      "status_change",
			FromState: "spawned",
			ToState:   "running",
			Timestamp: now,
		},
		{
			ID:        12,
			Source:    event.SourceDispatch,
			Type:      "status_change",
			FromState: "running",
			ToState:   "failed",
			Timestamp: now.Add(time.Second),
		},
		{
			ID:        13,
			Source:    event.SourceDispatch,
			Type:      "status_change",
			FromState: "failed",
			ToState:   "running",
			Timestamp: now.Add(2 * time.Second),
		},
		{
			ID:        14,
			Source:    event.SourceDispatch,
			Type:      "status_change",
			FromState: "running",
			ToState:   "completed",
			Timestamp: now.Add(3 * time.Second),
		},
	}

	eventID12 := int64(12)
	eventID13 := int64(13)
	inputs := []*Input{
		{ID: 100, RunID: "run2", Kind: "external", EventSource: event.SourceDispatch, EventID: &eventID12},
		{ID: 101, RunID: "run2", Kind: "external", EventSource: event.SourceDispatch, EventID: &eventID13},
	}

	got := BuildTimeline(events, inputs)
	if len(got) != 4 {
		t.Fatalf("timeline len=%d, want 4", len(got))
	}
	if got[1].ToState != "failed" || got[2].FromState != "failed" || got[2].ToState != "running" {
		t.Fatalf("expected explicit retry path around failure, got=%#v", got)
	}
	if len(got[1].InputIDs) != 1 || got[1].InputIDs[0] != 100 {
		t.Fatalf("failed step input linkage mismatch: %#v", got[1].InputIDs)
	}
	if len(got[2].InputIDs) != 1 || got[2].InputIDs[0] != 101 {
		t.Fatalf("retry step input linkage mismatch: %#v", got[2].InputIDs)
	}
}
