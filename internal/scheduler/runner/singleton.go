//go:build icsched_test && linux

package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"syscall"
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

	if o.hooks.beforeOpen != nil {
		o.hooks.beforeOpen()
	}
	d, err := db.OpenExisting(info.Canonical, o.BusyTimeout)
	if err != nil {
		return exitf(ExitRefused, "open %s: %w", info.Canonical, err)
	}
	defer d.Close()
	store := scheduler.NewStore(d.SqlDB())

	row, err := store.ReadRunner(ctx)
	if err != nil {
		return exitf(ExitRefused, "%w", err)
	}
	if o.hooks.afterOpen != nil {
		o.hooks.afterOpen()
	}

	// Both locks are taken before anything is written, and never released.
	dbLock, queueLock, err := acquireLocks(ctx, store, info.Canonical, row.QueueID, o)
	if err != nil {
		return err
	}
	defer queueLock.Close()
	defer dbLock.Close()
	if o.hooks.afterLocks != nil {
		o.hooks.afterLocks()
	}
	if err := verifyBinding(info, dbLock, o); err != nil {
		return err
	}

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

// verifyBinding makes the preflight verdict, the lock and the recorded
// identity refer to one file. The path is resolved separately by Preflight,
// by SQLite and by LockDB, so a swap between them could leave the lock on one
// inode and the connection on another, or skip the live-DB refusal. The lock
// descriptor must be the inode Preflight checked, and a second Preflight now
// must resolve to the same canonical path and inode. A swap after this check
// cannot matter: SQLite and the lock hold descriptors on the original inode.
func verifyBinding(info PathInfo, lock *os.File, o Options) error {
	st, err := lock.Stat()
	if err != nil {
		return exitf(ExitRefused, "stat database lock descriptor: %w", err)
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return exitf(ExitRefused, "stat database lock descriptor: unsupported platform")
	}
	if uint64(sys.Dev) != info.Dev || uint64(sys.Ino) != info.Ino {
		return exitf(ExitRefused, "database file changed during startup: %s was %s, the locked file is %d:%d", info.Canonical, info.Identity(), uint64(sys.Dev), uint64(sys.Ino))
	}
	if err := sqliteOnLockedFile(info, lock); err != nil {
		return err
	}
	again, err := Preflight(o.DB, o.AllowLive)
	if err != nil {
		return &ExitError{Code: ExitRefused, Err: err}
	}
	if again.Canonical != info.Canonical || again.Identity() != info.Identity() {
		return exitf(ExitRefused, "database file changed during startup: %s (%s) now resolves to %s (%s)", info.Canonical, info.Identity(), again.Canonical, again.Identity())
	}
	return nil
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

// drain marks the row draining and then stopped, on a fresh context because
// the run context is already cancelled. A false result means another runner
// already owns the row, so there is nothing to write. An error means the
// singleton still says "running" although this runner is leaving, so it is
// reported and the exit is not clean.
func drain(store *scheduler.Store, epoch int64, o Options) error {
	dctx, cancel := context.WithTimeout(context.Background(), o.BusyTimeout)
	defer cancel()
	for _, state := range []string{"draining", "stopped"} {
		if _, err := store.SetRunnerState(dctx, epoch, state); err != nil {
			return exitf(ExitRefused, "drain: could not mark the runner %s; the singleton row still says running: %w", state, err)
		}
	}
	return nil
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
			return drain(store, epoch, o)
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

// sqliteOnLockedFile checks that the SQLite connection holds the locked inode.
// The connection opens the path itself, so it can land on a different file than
// the one preflight checked and the lock holds (a rename or symlink swap between
// those steps); the lock-descriptor and path checks cannot see that. The
// connection is single and already used by now, so a descriptor on the locked
// inode other than the lock's own can only be SQLite's. The caller's reads have
// forced the connection open before this runs.
func sqliteOnLockedFile(info PathInfo, lock *os.File) error {
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return exitf(ExitRefused, "cannot inspect open descriptors to bind SQLite to the locked database: %w", err)
	}
	own := strconv.FormatUint(uint64(lock.Fd()), 10)
	for _, e := range ents {
		if e.Name() == own {
			continue
		}
		var st syscall.Stat_t
		if err := syscall.Stat("/proc/self/fd/"+e.Name(), &st); err != nil {
			continue // the directory handle used for this listing, or a closed descriptor
		}
		if uint64(st.Dev) == info.Dev && uint64(st.Ino) == info.Ino {
			return nil
		}
	}
	return exitf(ExitRefused, "database file changed during startup: SQLite did not open the locked file %s (%s)", info.Canonical, info.Identity())
}
