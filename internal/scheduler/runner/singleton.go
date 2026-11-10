//go:build icsched_test && linux

package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/mistakeknot/intercore/internal/db"
	"github.com/mistakeknot/intercore/internal/scheduler"
)

// maxHeartbeatErrors is how many consecutive heartbeat write errors (as
// opposed to a clean zero-row result) Run tolerates before it exits.
const maxHeartbeatErrors = 6

// Run is the runner skeleton of slice 3a: refusals, both lifetime locks, the
// epoch bump (or rebind), and the heartbeat. It claims nothing. It returns nil
// after a clean drain on ctx cancellation, or an *ExitError.
func Run(ctx context.Context, o Options) error {
	o.defaults()

	info, err := Preflight(o.DB, o.AllowLive)
	if err != nil {
		return &ExitError{Code: ExitRefused, Err: err}
	}
	netns, err := NetNS()
	if err != nil {
		return &ExitError{Code: ExitRefused, Err: err}
	}
	startTime, err := SelfStartTime()
	if err != nil {
		return &ExitError{Code: ExitRefused, Err: fmt.Errorf("read own start time: %w", err)}
	}

	d, err := db.Open(info.Canonical, o.BusyTimeout)
	if err != nil {
		return exitf(ExitRefused, "open %s: %w", info.Canonical, err)
	}
	defer d.Close()
	store := scheduler.NewStore(d.SqlDB())

	row, err := store.ReadRunner(ctx)
	if err != nil {
		return exitf(ExitRefused, "%w", err)
	}

	// Both locks are taken before anything is written, and never released.
	dbLock, queueLock, err := acquireLocks(ctx, store, info.Canonical, row.QueueID, o)
	if err != nil {
		return err
	}
	defer queueLock.Close()
	defer dbLock.Close()

	// Re-read after the locks: the row may have changed while we waited.
	row, err = store.ReadRunner(ctx)
	if err != nil {
		return exitf(ExitRefused, "%w", err)
	}

	runnerID, err := randomID()
	if err != nil {
		return exitf(ExitRefused, "runner id: %w", err)
	}
	host, _ := os.Hostname()
	id := scheduler.RunnerIdentity{
		RunnerID: runnerID, PID: os.Getpid(), PIDStartTime: startTime, BootID: BootID(),
		Host: host, Version: o.Version,
		DBPath: info.Canonical, DBIdentity: info.Identity(), NetNS: netns, UID: os.Getuid(),
	}

	var epoch int64
	if o.Rebind {
		epoch, err = rebind(ctx, store, row, id, o)
	} else {
		epoch, _, err = store.AcquireEpoch(ctx, id, o.Now())
		if errors.Is(err, scheduler.ErrIdentityMismatch) {
			err = &ExitError{Code: ExitRefused, Err: err}
		}
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(o.Stderr, "ic scheduler run: epoch %d, queue %s, db %s\n", epoch, row.QueueID, info.Canonical)
	if o.OnReady != nil {
		o.OnReady(epoch)
	}
	return heartbeatLoop(ctx, store, epoch, o)
}

func randomID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// acquireLocks takes lock 1 (flock on the DB inode) and then lock 2 (the
// queue socket). When a lock is held and the recorded holder is verified
// alive, it exits 3 at once. Otherwise the holder may be exiting, so it retries
// until LockWait runs out and exits 1.
func acquireLocks(ctx context.Context, store *scheduler.Store, canonical, queueID string, o Options) (dbLock *os.File, queueLock interface{ Close() error }, err error) {
	deadline := o.Now().Add(o.LockWait)
	for {
		if dbLock == nil {
			f, lerr := LockDB(canonical)
			switch {
			case lerr == nil:
				dbLock = f
			case errors.Is(lerr, ErrHeld):
				if err := holderCheck(ctx, store, canonical); err != nil {
					return nil, nil, err
				}
			default:
				return nil, nil, &ExitError{Code: ExitRefused, Err: lerr}
			}
		}
		if dbLock != nil {
			ln, lerr := LockQueue(queueID)
			switch {
			case lerr == nil:
				return dbLock, ln, nil
			case errors.Is(lerr, ErrHeld):
				if err := holderCheck(ctx, store, canonical); err != nil {
					dbLock.Close()
					return nil, nil, err
				}
			default:
				dbLock.Close()
				return nil, nil, &ExitError{Code: ExitRefused, Err: lerr}
			}
		}
		if !o.Now().Before(deadline) {
			if dbLock != nil {
				dbLock.Close()
				return nil, nil, exitf(ExitRefused, "queue lock held by another process (no live runner is recorded; a squatter holds the abstract socket, see ss -xlp)")
			}
			return nil, nil, exitf(ExitRefused, "database lock on %s held by another process, and no live runner is recorded", canonical)
		}
		select {
		case <-ctx.Done():
			if dbLock != nil {
				dbLock.Close()
			}
			return nil, nil, &ExitError{Code: ExitRefused, Err: ctx.Err()}
		case <-time.After(o.RetryEvery):
		}
	}
}

// holderCheck exits 3 when the singleton's recorded runner is verified alive.
func holderCheck(ctx context.Context, store *scheduler.Store, canonical string) error {
	row, err := store.ReadRunner(ctx)
	if err != nil {
		return nil // cannot verify; the caller keeps retrying
	}
	h := Holder{PID: row.PID, StartTime: row.PIDStartTime, BootID: row.BootID}
	if row.State != "stopped" && h.Present(BootID(), ReadStat) {
		return exitf(ExitSuperseded, "another runner (pid %d) holds %s", row.PID, canonical)
	}
	return nil
}

// rebind runs --rebind-db: both locks are already held. It refuses unless the
// operator confirmed the old runner is dead and the recorded runner is absent
// on this host (a stopped or zombie process counts as present).
func rebind(ctx context.Context, store *scheduler.Store, row *scheduler.RunnerRow, id scheduler.RunnerIdentity, o Options) (int64, error) {
	if !o.ConfirmOldRunnerDead {
		return 0, exitf(ExitRefused, "--rebind-db needs --confirm-old-runner-dead: only the operator can say the old runner, on any host or namespace, is dead")
	}
	h := Holder{PID: row.PID, StartTime: row.PIDStartTime, BootID: row.BootID}
	if h.Present(id.BootID, ReadStat) {
		return 0, exitf(ExitRefused, "refusing to rebind: the recorded runner (pid %d) is still present on this host", row.PID)
	}
	res, err := store.Rebind(ctx, scheduler.RebindParams{Identity: id, Now: o.Now()})
	if err != nil {
		return 0, exitf(ExitRefused, "rebind: %w", err)
	}
	fmt.Fprintf(o.Stderr, "ic scheduler run: rebound to %s; scheduler paused; %d in-flight rows quarantined as rebind_unknown\n", id.DBPath, res.Quarantined)
	return res.Epoch, nil
}

// heartbeatLoop refreshes heartbeat_at until ctx is cancelled (a clean drain)
// or the epoch is no longer ours (exit 3).
func heartbeatLoop(ctx context.Context, store *scheduler.Store, epoch int64, o Options) error {
	t := time.NewTicker(o.HeartbeatEvery)
	defer t.Stop()
	errs := 0
	for {
		select {
		case <-ctx.Done():
			// A drain with nothing in flight: mark the row and stop. A false
			// result means we were already superseded; nothing to write.
			dctx, cancel := context.WithTimeout(context.Background(), o.BusyTimeout)
			defer cancel()
			store.SetRunnerState(dctx, epoch, "draining")
			store.SetRunnerState(dctx, epoch, "stopped")
			return nil
		case <-t.C:
			ok, err := store.Heartbeat(ctx, epoch, o.Now())
			switch {
			case err != nil:
				errs++
				fmt.Fprintf(o.Stderr, "ic scheduler run: heartbeat error (%d): %v\n", errs, err)
				if errs >= maxHeartbeatErrors {
					return exitf(ExitRefused, "heartbeat failed %d times in a row: %w", errs, err)
				}
			case !ok:
				return exitf(ExitSuperseded, "superseded: epoch %d is no longer current", epoch)
			default:
				errs = 0
			}
		}
	}
}
