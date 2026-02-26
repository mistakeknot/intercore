// internal/observability/observability_test.go
package observability

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"testing"
)

func TestTraceFromEnv_AllSet(t *testing.T) {
	t.Setenv("IC_TRACE_ID", "abcdef1234567890abcdef1234567890")
	t.Setenv("IC_SPAN_ID", "1234567890abcdef")
	t.Setenv("IC_PARENT_SPAN_ID", "fedcba0987654321")

	tc := TraceFromEnv()
	if tc.TraceID != "abcdef1234567890abcdef1234567890" {
		t.Errorf("TraceID = %q, want abcdef...", tc.TraceID)
	}
	if tc.SpanID != "1234567890abcdef" {
		t.Errorf("SpanID = %q, want 1234...", tc.SpanID)
	}
	if tc.ParentSpanID != "fedcba0987654321" {
		t.Errorf("ParentSpanID = %q, want fedcba...", tc.ParentSpanID)
	}
}

func TestTraceFromEnv_Empty(t *testing.T) {
	os.Unsetenv("IC_TRACE_ID")
	os.Unsetenv("IC_SPAN_ID")
	os.Unsetenv("IC_PARENT_SPAN_ID")

	tc := TraceFromEnv()
	if tc.TraceID != "" {
		t.Errorf("TraceID = %q, want empty", tc.TraceID)
	}
}

func TestGenerateTraceID(t *testing.T) {
	id := GenerateTraceID()
	if len(id) != 32 {
		t.Errorf("TraceID length = %d, want 32", len(id))
	}
	// Verify hex
	for _, c := range id {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("TraceID contains non-hex char: %c", c)
		}
	}
}

func TestGenerateSpanID(t *testing.T) {
	id := GenerateSpanID()
	if len(id) != 16 {
		t.Errorf("SpanID length = %d, want 16", len(id))
	}
}

func TestNewHandler_InjectsTraceContext(t *testing.T) {
	t.Setenv("IC_TRACE_ID", "aaaa1111bbbb2222cccc3333dddd4444")
	t.Setenv("IC_SPAN_ID", "eeee5555ffff6666")

	var buf bytes.Buffer
	logger := slog.New(NewHandler(&buf, slog.LevelDebug))
	logger.Info("test message", "key", "value")

	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("unmarshal log: %v", err)
	}
	if record["trace_id"] != "aaaa1111bbbb2222cccc3333dddd4444" {
		t.Errorf("trace_id = %v, want aaaa...", record["trace_id"])
	}
	if record["span_id"] != "eeee5555ffff6666" {
		t.Errorf("span_id = %v, want eeee...", record["span_id"])
	}
	if record["key"] != "value" {
		t.Errorf("key = %v, want value", record["key"])
	}
}

func TestNewHandler_NoTraceContext(t *testing.T) {
	os.Unsetenv("IC_TRACE_ID")
	os.Unsetenv("IC_SPAN_ID")

	var buf bytes.Buffer
	logger := slog.New(NewHandler(&buf, slog.LevelDebug))
	logger.Info("test")

	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("unmarshal log: %v", err)
	}
	// trace_id should not be present when env var is unset
	if _, ok := record["trace_id"]; ok {
		t.Error("trace_id present when IC_TRACE_ID not set")
	}
}

func TestNewHandler_LevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(NewHandler(&buf, slog.LevelWarn))
	logger.Info("should be filtered")
	if buf.Len() > 0 {
		t.Error("Info message should be filtered at Warn level")
	}
	logger.Warn("should appear")
	if buf.Len() == 0 {
		t.Error("Warn message should appear at Warn level")
	}
}
