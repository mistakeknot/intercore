package event

import (
	"context"
	"fmt"
	"io"
	"os"
)

// NewLogHandler returns a handler that prints structured event lines.
// If quiet is true, logs are suppressed.
func NewLogHandler(w io.Writer, quiet bool) Handler {
	if w == nil {
		w = os.Stderr
	}
	return func(ctx context.Context, e Event) error {
		if quiet {
			return nil
		}
		fmt.Fprintf(w, "[event] source=%s type=%s run=%s from=%s to=%s",
			e.Source, e.Type, e.RunID, e.FromState, e.ToState)
		if e.Reason != "" {
			fmt.Fprintf(w, " reason=%q", e.Reason)
		}
		fmt.Fprintln(w)
		return nil
	}
}
