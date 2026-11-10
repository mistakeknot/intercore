//go:build icsched_test && !linux

package runner

import (
	"context"
	"errors"
)

// The runner is Linux-only (abstract sockets, flock semantics, /proc). Under
// the icsched_test tag on another OS it compiles but refuses to run.

// Run always refuses off Linux.
func Run(_ context.Context, _ Options) error {
	return &ExitError{Code: ExitRefused, Err: errors.New("the scheduler runner supports Linux only")}
}
