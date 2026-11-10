//go:build !icsched_test

package dispatch

import "context"

// The release build has no seams: these are empty and inline away.

func seam(context.Context, string, string) {}

func startFault(context.Context) error { return nil }
