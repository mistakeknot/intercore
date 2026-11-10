//go:build !icsched_test

package main

import (
	"context"
	"fmt"
	"os"
)

// cmdSchedulerRunner is the release build's `ic scheduler runner`. It exits
// before it reads a flag, resolves a database path or opens a file.
func cmdSchedulerRunner(_ context.Context, _ []string) int {
	fmt.Fprintln(os.Stderr, "ic: scheduler runner not enabled in this build")
	return 2
}
