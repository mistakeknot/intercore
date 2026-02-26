// internal/observability/observability.go
package observability

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
)

// TraceContext holds distributed trace identifiers propagated via environment.
type TraceContext struct {
	TraceID      string
	SpanID       string
	ParentSpanID string
}

// TraceFromEnv reads trace context from IC_TRACE_ID, IC_SPAN_ID, IC_PARENT_SPAN_ID.
func TraceFromEnv() TraceContext {
	return TraceContext{
		TraceID:      os.Getenv("IC_TRACE_ID"),
		SpanID:       os.Getenv("IC_SPAN_ID"),
		ParentSpanID: os.Getenv("IC_PARENT_SPAN_ID"),
	}
}

// GenerateTraceID returns a 32-char lowercase hex string (128-bit, OTel-compatible).
func GenerateTraceID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// GenerateSpanID returns a 16-char lowercase hex string (64-bit, OTel-compatible).
func GenerateSpanID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// ParseLevel maps a string to slog.Level. Returns slog.LevelWarn for unrecognized values.
func ParseLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelWarn
	}
}

// NewHandler returns a slog.JSONHandler that auto-injects trace context from env.
// Trace attributes are only added when IC_TRACE_ID is set.
func NewHandler(w io.Writer, level slog.Level) slog.Handler {
	opts := &slog.HandlerOptions{Level: level}
	base := slog.NewJSONHandler(w, opts)

	tc := TraceFromEnv()
	if tc.TraceID == "" {
		return base
	}

	attrs := []slog.Attr{
		slog.String("trace_id", tc.TraceID),
	}
	if tc.SpanID != "" {
		attrs = append(attrs, slog.String("span_id", tc.SpanID))
	}
	if tc.ParentSpanID != "" {
		attrs = append(attrs, slog.String("parent_span_id", tc.ParentSpanID))
	}

	// WithAttrs returns a new handler that prepends these attrs to every record
	return base.WithAttrs(attrs)
}
