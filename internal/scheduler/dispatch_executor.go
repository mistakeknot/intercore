package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mistakeknot/intercore/internal/dispatch"
)

// DispatchExecutor supplies the governed boundary for an explicitly configured
// Scheduler. Hosts still own queue loading and persistence hooks; no daemon is
// started here. A job is attempted once and its actual dispatch is collected.
func DispatchExecutor(store *dispatch.Store) SpawnExecutor {
	return func(ctx context.Context, job *SpawnJob) error {
		job.mu.Lock()
		job.MaxRetries = job.RetryCount // Never turn a lost acknowledgement into a replay.
		alreadyDispatched := job.DispatchID != ""
		job.mu.Unlock()
		if alreadyDispatched || job.Type != JobTypeDispatch {
			return errors.New("scheduled dispatch must be a fresh dispatch job")
		}
		var opts dispatch.SpawnOptions
		decoder := json.NewDecoder(strings.NewReader(job.SpawnOpts))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&opts); err != nil {
			return fmt.Errorf("scheduled dispatch options: %w", err)
		}
		if job.AgentType != "" && opts.AgentType != job.AgentType {
			return errors.New("scheduled dispatch backend identity mismatch")
		}
		// This is the actual atomic-admission path, evaluated at execution time.
		result, err := dispatch.Spawn(ctx, store, opts)
		if err != nil {
			return err
		}
		job.mu.Lock()
		job.DispatchID = result.ID
		job.mu.Unlock()
		done := make(chan error, 1)
		go func() { done <- result.Cmd.Wait() }()
		var waitErr error
		select {
		case waitErr = <-done:
		case <-ctx.Done():
			if err := result.Terminate(); err != nil {
				// Persist the detached ownership rejection; do not wait forever on
				// a process we cannot safely signal or replay the admitted attempt.
				_ = dispatch.Kill(context.Background(), store, result.ID)
				return fmt.Errorf("scheduled cancellation: %w", err)
			}
			<-done
			waitErr = ctx.Err()
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := dispatch.Collect(cleanup, store, result.ID); err != nil {
			return err
		}
		if waitErr != nil {
			return waitErr
		}
		terminal, err := store.Get(cleanup, result.ID)
		if err != nil {
			return err
		}
		if terminal.Status != dispatch.StatusCompleted {
			return fmt.Errorf("scheduled dispatch %s: %s", result.ID, terminal.Status)
		}
		return nil
	}
}
