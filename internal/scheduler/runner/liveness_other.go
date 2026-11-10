//go:build icsched_test && !linux

package runner

import (
	"context"
	"errors"
	"time"

	"github.com/mistakeknot/intercore/internal/scheduler"
)

// RunnerStatus is what `ic scheduler runner` reports.
type RunnerStatus struct {
	Liveness     string
	State        string
	PID          int
	Epoch        int64
	Version      string
	HeartbeatAge time.Duration
	Paused       bool
	InFlight     int
	Queue        map[string]int
}

// Probe is unsupported off Linux: the process table check needs /proc.
func Probe(context.Context, *scheduler.Store, time.Time, time.Duration) (*RunnerStatus, error) {
	return nil, errors.New("scheduler runner status needs Linux")
}
