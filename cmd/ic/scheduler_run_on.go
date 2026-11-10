//go:build icsched_test

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mistakeknot/intercore/internal/cli"
	"github.com/mistakeknot/intercore/internal/scheduler/runner"
)

// cmdSchedulerRun is `ic scheduler run`, compiled only under the icsched_test
// build tag (plan section 12, slice 3). In slice 3a it validates the database,
// takes both lifetime locks, bumps the epoch (or rebinds) and heartbeats. It
// claims nothing: claim and launch arrive in a later slice.
//
// Exit codes: 0 clean drain; 1 refusal; 3 a verified live runner exists or this
// runner was superseded; 5 invariant violated.
func cmdSchedulerRun(ctx context.Context, args []string) int {
	f := cli.ParseFlags(args)
	if bad := f.Unknown("rebind-db", "confirm-old-runner-dead", "lock-wait", "heartbeat-interval"); len(bad) > 0 {
		fmt.Fprintf(os.Stderr, "ic: scheduler run: unknown flag --%s\n", bad[0])
		return runner.ExitRefused
	}
	lockWait, err := f.Duration("lock-wait", 30*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: scheduler run: %v\n", err)
		return runner.ExitRefused
	}
	heartbeat, err := f.Duration("heartbeat-interval", 5*time.Second)
	if err != nil || heartbeat <= 0 {
		fmt.Fprintf(os.Stderr, "ic: scheduler run: invalid --heartbeat-interval\n")
		return runner.ExitRefused
	}
	path, err := resolveDBPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: scheduler run: %v\n", err)
		return runner.ExitRefused
	}

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	err = runner.Run(ctx, runner.Options{
		DB:                   path,
		AllowLive:            os.Getenv("IC_SCHEDULER_ALLOW_LIVE") == "1",
		Rebind:               f.Bool("rebind-db"),
		ConfirmOldRunnerDead: f.Bool("confirm-old-runner-dead"),
		LockWait:             lockWait,
		HeartbeatEvery:       heartbeat,
		BusyTimeout:          flagTimeout,
		Stderr:               os.Stderr,
	})
	if err == nil {
		return 0
	}
	fmt.Fprintf(os.Stderr, "ic: scheduler run: %v\n", err)
	var ee *runner.ExitError
	if errors.As(err, &ee) {
		return ee.Code
	}
	return runner.ExitRefused
}
