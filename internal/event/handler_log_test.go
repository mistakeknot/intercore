package event

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestLogHandler_OutputsStructuredLine(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := NewLogHandler(logger)

	e := Event{
		Source:    SourcePhase,
		Type:      "advance",
		RunID:     "abc12345",
		FromState: "brainstorm",
		ToState:   "strategized",
		Reason:    "Gate passed",
		Timestamp: time.Now(),
	}

	if err := h(context.Background(), e); err != nil {
		t.Fatal(err)
	}

	line := buf.String()
	// Verify structured JSON output contains expected fields
	var record map[string]any
	if err := json.Unmarshal([]byte(line), &record); err != nil {
		t.Fatalf("unmarshal log: %v (line: %s)", err, line)
	}
	if record["msg"] != "event" {
		t.Errorf("msg = %v, want event", record["msg"])
	}
	if record["source"] != "phase" {
		t.Errorf("source = %v, want phase", record["source"])
	}
	if record["run_id"] != "abc12345" {
		t.Errorf("run_id = %v, want abc12345", record["run_id"])
	}
	if record["reason"] != "Gate passed" {
		t.Errorf("reason = %v, want Gate passed", record["reason"])
	}
}

func TestLogHandler_Quiet(t *testing.T) {
	h := NewLogHandler(nil) // nil logger = quiet mode

	err := h(context.Background(), Event{Source: SourcePhase, Type: "advance"})
	if err != nil {
		t.Fatal(err)
	}
	// No output check needed — nil logger means no output destination to check
}

func TestLogHandler_NoReason(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := NewLogHandler(logger)

	h(context.Background(), Event{Source: SourceDispatch, Type: "status_change", RunID: "r1", FromState: "spawned", ToState: "running"})

	line := buf.String()
	if strings.Contains(line, `"reason"`) {
		t.Errorf("should not have reason field: %s", line)
	}
}
