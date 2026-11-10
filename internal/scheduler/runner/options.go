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

	// The claim and launch loop (slice 3b).
	DispatchSH    string        // absolute path of the executor every job runs; required to run any job
	MaxConcurrent int           // jobs launched at once; default 2
	Tick          time.Duration // claim and completion cadence; default 1s
	DrainTimeout  time.Duration // how long a SIGTERM drain waits for running jobs; default 30s
	RepairEvery   time.Duration // current-epoch repair pass cadence; default 30s
	RetryBase     time.Duration // first backoff after a failed completion step; default 1s
	RetryMax      time.Duration // backoff ceiling; default 30s

	// OnReady runs once the epoch is bumped and the runner is live. Test seam.
	OnReady func(epoch int64)

	// hooks are in-package test seams for races that need a deterministic
	// interleaving; they exist only in files built under icsched_test.
	hooks struct {
		beforeOpen func() // after preflight, before SQLite opens the file
		afterOpen  func() // after SQLite opens the file, before the locks
		afterLocks func() // after both locks, before the binding check

		// Engine seams (slice 3b). Each runs on the goroutine that reached
		// the named point, so a test can hold or drop state deterministically.
		onInFlight                 func(*InFlight)            // once, with the in-flight map, before the first tick
		claimPass                  func(paused bool)          // at the start of every claim pass, with the pause flag it read
		afterCandidates            func(ids []string)         // between candidate selection and the claim
		beforeSpawn                func(token string)         // after the claim-time checks and attempt paths, before Spawn
		afterLaunchPersist         func(token, result string) // right after launch_result commits
		afterExitBeforeEvidence    func(token string)         // right after exited_at commits, before the entry moves to completing
		afterEvidenceBeforeCollect func(token string)         // after the evidence write, before Collect
		inject                     func(kind string) error    // kinds: marker, evidence, collect, finalize; a non-nil error fails that step
		failStop                   func(token, stage string)  // replaces the exit-5 fail-stop
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
	if o.MaxConcurrent == 0 {
		o.MaxConcurrent = 2
	}
	if o.Tick == 0 {
		o.Tick = time.Second
	}
	if o.DrainTimeout == 0 {
		o.DrainTimeout = 30 * time.Second
	}
	if o.RepairEvery == 0 {
		o.RepairEvery = 30 * time.Second
	}
	if o.RetryBase == 0 {
		o.RetryBase = time.Second
	}
	if o.RetryMax == 0 {
		o.RetryMax = 30 * time.Second
	}
}
