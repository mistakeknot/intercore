//go:build icsched_test

package runner

import (
	"fmt"
	"io"
	"time"
)

// Exit codes of `ic scheduler run` (plan section 5.3).
const (
	ExitRefused    = 1 // any refusal not listed below
	ExitNotEnabled = 2 // release build
	ExitSuperseded = 3 // a verified live runner exists, or this one was superseded
	ExitInvariant  = 5 // a fail-stop
)

// ExitError carries the process exit code Run wants.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string { return e.Err.Error() }
func (e *ExitError) Unwrap() error { return e.Err }

func exitf(code int, format string, a ...any) error {
	return &ExitError{Code: code, Err: fmt.Errorf(format, a...)}
}

// Options configures Run.
type Options struct {
	DB                   string
	AllowLive            bool          // IC_SCHEDULER_ALLOW_LIVE=1
	Rebind               bool          // --rebind-db
	ConfirmOldRunnerDead bool          // --confirm-old-runner-dead
	LockWait             time.Duration // how long to retry a held lock whose holder is not verified alive; default 30s
	RetryEvery           time.Duration // pause between lock attempts; default 250ms
	HeartbeatEvery       time.Duration // default 5s
	BusyTimeout          time.Duration // SQLite busy timeout; default 5s
	Version              string
	Stderr               io.Writer
	Now                  func() time.Time

	// OnReady runs once the epoch is bumped and the runner is live. Test seam.
	OnReady func(epoch int64)

	// hooks are in-package test seams for races that need a deterministic
	// interleaving; they exist only in files built under icsched_test.
	hooks struct {
		beforeOpen func() // after preflight, before SQLite opens the file
		afterOpen  func() // after SQLite opens the file, before the locks
		afterLocks func() // after both locks, before the binding check
	}
}

func (o *Options) defaults() {
	if o.LockWait == 0 {
		o.LockWait = 30 * time.Second
	}
	if o.RetryEvery == 0 {
		o.RetryEvery = 250 * time.Millisecond
	}
	if o.HeartbeatEvery == 0 {
		o.HeartbeatEvery = 5 * time.Second
	}
	if o.BusyTimeout == 0 {
		o.BusyTimeout = 5 * time.Second
	}
	if o.Version == "" {
		o.Version = "icsched_test"
	}
	if o.Stderr == nil {
		o.Stderr = io.Discard
	}
	if o.Now == nil {
		o.Now = time.Now
	}
}
