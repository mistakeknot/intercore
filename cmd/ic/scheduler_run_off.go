//go:build !icsched_test

package main

import (
	"context"
	"fmt"
	"os"
)

// cmdSchedulerRun is the release build's `ic scheduler run`. The runner is
// compiled only under the icsched_test build tag, so a shipped binary has no
// execution code behind this command. It exits before it reads any flag,
// resolves a database path, opens a file, takes a lock or reads the
// environment, so it touches nothing.
func cmdSchedulerRun(_ context.Context, _ []string) int {
	fmt.Fprintln(os.Stderr, "ic: scheduler runner not enabled in this build")
	return 2
}
