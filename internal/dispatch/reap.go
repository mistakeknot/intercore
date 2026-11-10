package dispatch

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// DefaultReservationLease is how long a dispatch row may sit in "spawned" with
// no process before Reap treats it as an abandoned reservation. Spawn records a
// process within moments of admitting a row, so only a row nothing can start
// (a retry reservation, or a spawn that died between admission and exec) ages
// past it.
const DefaultReservationLease = 10 * time.Minute

// ReapResult reports what a Reap pass did.
type ReapResult struct {
	// Collected are dispatches whose process had exited; their outcome was
	// recorded through Collect, as Poll would have.
	Collected []string
	// Released are abandoned reservations that were cancelled, freeing the
	// active-dispatch slots they held.
	Released []string
}

// Total is the number of dispatches Reap moved to a terminal status.
func (r *ReapResult) Total() int { return len(r.Collected) + len(r.Released) }

// StopByRun ends the processes of the run's unsupervised active dispatches and
// records each as cancelled. It signals the whole process group, exactly as
// Kill does, and only after the recorded birth identity proves the process is
// still the one that was admitted. Supervised dispatches are left alone: their
// supervisor is the only writer of their outcome (CancelByRun queues a
// run_rollback intent for them). Rows with no process are left for CancelByRun.
// It returns the number of dispatches it terminalized.
func StopByRun(ctx context.Context, store *Store, runID string) (int64, error) {
	rows, err := store.db.QueryContext(ctx,
		`SELECT id FROM dispatches WHERE scope_id = ? AND pid IS NOT NULL AND status IN ('spawned', 'running')`, runID)
	if err != nil {
		return 0, fmt.Errorf("stop dispatches for run: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("stop dispatches for run: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("stop dispatches for run: %w", err)
	}

	var stopped int64
	var firstErr error
	for _, id := range ids {
		d, err := store.Get(ctx, id)
		if err != nil {
			return stopped, err
		}
		if d.IsTerminal() {
			continue
		}
		err = interruptDispatch(ctx, store, d, StatusCancelled, "run "+runID+" rolled back")
		switch {
		case err == nil, IsSpawnRejection(err):
			// A rejection means ownership could not be proven; the row was
			// terminalized as indeterminate without signalling anything.
			stopped++
		case errors.Is(err, ErrSupervised), errors.Is(err, ErrStaleStatus):
			// The supervisor owns it, or another writer finished it first.
		default:
			if firstErr == nil {
				firstErr = fmt.Errorf("stop dispatch %s: %w", id, err)
			}
		}
	}
	return stopped, firstErr
}

// Reap moves dead or abandoned active dispatches to a terminal status without
// waiting for someone to poll them.
//
//   - A running or spawned dispatch whose process has exited (proved by its
//     birth identity, never by the bare PID) is collected, as Poll does.
//   - A spawned dispatch with no process that is older than lease is cancelled,
//     releasing its slot. A lease <= 0 uses DefaultReservationLease.
//
// A dispatch whose liveness cannot be proved, and a supervised dispatch, is left
// as it is. Reap never signals a process.
func (s *Store) Reap(ctx context.Context, lease time.Duration) (*ReapResult, error) {
	active, err := s.ListActive(ctx)
	if err != nil {
		return nil, err
	}
	result := &ReapResult{}
	for _, d := range active {
		if err := s.reapOne(ctx, d, lease, true, result); err != nil {
			return result, err
		}
	}
	return result, nil
}

// ReapDispatch collects one dispatch whose process has exited. Unlike Reap it
// never releases a reservation: a read of a single row must not rewrite a row
// that has no process.
func (s *Store) ReapDispatch(ctx context.Context, id string) (*ReapResult, error) {
	d, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	result := &ReapResult{}
	if d.IsTerminal() {
		return result, nil
	}
	return result, s.reapOne(ctx, d, 0, false, result)
}

func (s *Store) reapOne(ctx context.Context, d *Dispatch, lease time.Duration, releaseReservations bool, result *ReapResult) error {
	if supervised, err := isSupervised(ctx, s.db, d.ID); err != nil {
		return err
	} else if supervised {
		return nil
	}
	if lease <= 0 {
		lease = DefaultReservationLease
	}

	if d.PID == nil {
		if !releaseReservations || d.Status != StatusSpawned || time.Since(time.Unix(d.CreatedAt, 0)) < lease {
			return nil
		}
		reason := "reservation never started a process"
		// The status, pid and age checks run again inside the write transaction:
		// a spawn that recorded its process after d was read must not be cancelled.
		_, err := s.Terminalize(ctx, d.ID, Terminal{
			Status:            StatusCancelled,
			Source:            TerminalSourceKill,
			Evidence:          EvidenceNotApplicable,
			Reason:            reason,
			Fields:            UpdateFields{"completed_at": time.Now().Unix(), "error_message": reason},
			ReservationCutoff: time.Now().Add(-lease).Unix(),
		})
		if errors.Is(err, ErrPreconditionFailed) {
			return nil
		}
		return reaped(&result.Released, d.ID, err)
	}

	// Liveness is proved only by the recorded birth identity, never by the bare
	// PID: a legacy row without one, or one that cannot be read, is left alone.
	identity, err := s.processIdentity(ctx, d)
	if err != nil {
		return nil
	}
	alive, err := processIdentityAlive(identity)
	if err != nil || alive {
		return nil
	}
	return reaped(&result.Collected, d.ID, Collect(ctx, s, d.ID))
}

// reaped records id when the terminal write succeeded or lost a race to another
// writer; either way the dispatch is no longer active.
func reaped(into *[]string, id string, err error) error {
	if err != nil && !errors.Is(err, ErrStaleStatus) {
		return fmt.Errorf("reap %s: %w", id, err)
	}
	if err == nil {
		*into = append(*into, id)
	}
	return nil
}
