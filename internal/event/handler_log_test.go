package event

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func TestLogHandler_OutputsStructuredLine(t *testing.T) {
	var buf bytes.Buffer
	h := NewLogHandler(&buf, false)

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
	for _, want := range []string{"[event]", "source=phase", "run=abc12345", `reason="Gate passed"`} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q in output: %s", want, line)
		}
	}
}

func TestLogHandler_Quiet(t *testing.T) {
	var buf bytes.Buffer
	h := NewLogHandler(&buf, true)

	h(context.Background(), Event{Source: SourcePhase, Type: "advance"})

	if buf.Len() != 0 {
		t.Errorf("quiet handler produced output: %s", buf.String())
	}
}

func TestLogHandler_NoReason(t *testing.T) {
	var buf bytes.Buffer
	h := NewLogHandler(&buf, false)

	h(context.Background(), Event{Source: SourceDispatch, Type: "status_change", RunID: "r1", FromState: "spawned", ToState: "running"})

	line := buf.String()
	if strings.Contains(line, "reason=") {
		t.Errorf("should not have reason field: %s", line)
	}
}
