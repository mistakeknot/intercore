package event

import (
	"context"
	"log/slog"
)

// NewLogHandler returns a handler that logs structured event lines.
// If logger is nil, logging is suppressed (equivalent to the old quiet=true).
func NewLogHandler(logger *slog.Logger) Handler {
	return func(ctx context.Context, e Event) error {
		if logger == nil {
			return nil
		}
		attrs := []slog.Attr{
			slog.String("source", e.Source),
			slog.String("type", e.Type),
			slog.String("run_id", e.RunID),
			slog.String("from", e.FromState),
			slog.String("to", e.ToState),
		}
		if e.Reason != "" {
			attrs = append(attrs, slog.String("reason", e.Reason))
		}
		logger.LogAttrs(ctx, slog.LevelInfo, "event", attrs...)
		return nil
	}
}
