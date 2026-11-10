//go:build icsched_test

package scheduler

import "context"

// A test seam for the runner's barrier 9 (in_finalize_before_commit). It exists
// only under the icsched_test build tag; seam_off.go is the release build.

type seamKey struct{}

// WithSeam returns a context whose Finalize calls fn at the named point, inside
// the open transaction and on the calling goroutine. fn may block.
func WithSeam(ctx context.Context, fn func(point string)) context.Context {
	return context.WithValue(ctx, seamKey{}, fn)
}

func seam(ctx context.Context, point string) {
	if fn, _ := ctx.Value(seamKey{}).(func(point string)); fn != nil {
		fn(point)
	}
}
