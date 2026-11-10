//go:build !icsched_test

package scheduler

import "context"

// The release build has no seams.
func seam(context.Context, string) {}
