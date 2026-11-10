//go:build icsched_test && linux

package runner

import (
	"context"
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
	InFlight     int // jobs with status running
	Queue        map[string]int
}

// Probe reads the singleton row and the process table. It takes no lock, so it
// can never delay a runner restart.
func Probe(ctx context.Context, store *scheduler.Store, now time.Time, staleAfter time.Duration) (*RunnerStatus, error) {
	row, err := store.ReadRunner(ctx)
	if err != nil {
		return nil, err
	}
	paused, err := store.IsPaused(ctx)
	if err != nil {
		return nil, err
	}
	counts, err := store.CountByStatus(ctx)
	if err != nil {
		return nil, err
	}
	st := &RunnerStatus{State: row.State, PID: row.PID, Epoch: row.Epoch, Version: row.Version, Paused: paused, Queue: counts, InFlight: counts["running"]}
	if row.HeartbeatAt > 0 {
		st.HeartbeatAge = now.Sub(time.Unix(row.HeartbeatAt, 0))
	}
	present := Holder{PID: row.PID, StartTime: row.PIDStartTime, BootID: row.BootID}.Present(BootID(), ReadStat)
	st.Liveness = ClassifyLiveness(row.State, st.HeartbeatAge, staleAfter, present)
	return st, nil
}
