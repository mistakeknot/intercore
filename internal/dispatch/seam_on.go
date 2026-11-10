//go:build icsched_test

package dispatch

import "context"

// Test seams for the scheduler runner (plan section 9, barriers 3 and 4). They
// exist only under the icsched_test build tag; a release build has the no-op
// in seam_off.go and none of this code.

type seamKey struct{}

// WithSeam returns a context whose Spawn calls fn at each named point, on the
// calling goroutine. fn may block: Spawn waits for it.
func WithSeam(ctx context.Context, fn func(point, dispatchID string)) context.Context {
	return context.WithValue(ctx, seamKey{}, fn)
}

func seam(ctx context.Context, point, id string) {
	if fn, _ := ctx.Value(seamKey{}).(func(point, dispatchID string)); fn != nil {
		fn(point, id)
	}
}

// StartFault, when it returns an error, replaces cmd.Start in the next Spawn
// of a context that carries it (fault hook: inject EAGAIN from Start).
type startFaultKey struct{}

// WithStartFault returns a context whose Spawn fails at Start with the error
// fn returns, when fn returns non-nil, without starting a process.
func WithStartFault(ctx context.Context, fn func() error) context.Context {
	return context.WithValue(ctx, startFaultKey{}, fn)
}

func startFault(ctx context.Context) error {
	if fn, _ := ctx.Value(startFaultKey{}).(func() error); fn != nil {
		return fn()
	}
	return nil
}
