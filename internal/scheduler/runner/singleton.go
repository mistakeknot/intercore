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

// Run takes the refusals, both lifetime locks and the epoch (or rebind), then
// hands over to the engine: heartbeat, claim, launch, complete. It returns nil
// after a clean drain on ctx cancellation, or an *ExitError.
func Run(ctx context.Context, o Options) error {
	o.defaults()
	o.installEnvHooks()

	// Containment is checked first, before any lock or epoch: a runner that
	// cannot contain its jobs refuses without touching the database. The
	// explicit fallback is --no-containment.
	var cg *cgroupMgr
	if !o.NoContainment {
		var err error
		if cg, err = newCgroupMgr(o); err != nil {
			return exitf(ExitRefused, "containment is not available (%w); delegate a cgroup v2 subtree to this user, or pass --no-containment to run without it", err)
		}
		defer cg.release()
	}

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
	fdsBefore, err := snapshotFDs()
	if err != nil {
		return exitf(ExitRefused, "cannot inspect open descriptors to bind SQLite to the locked database: %w", err)
	}
	if err := refusePreopened(info, fdsBefore); err != nil {
		return err
	}
	d, err := db.OpenExisting(info.Canonical, o.BusyTimeout)
	if err != nil {
		return exitf(ExitRefused, "open %s: %w", info.Canonical, err)
	}
	defer d.Close()
	store := scheduler.NewStore(d.SqlDB())

	if o.hooks.afterOpen != nil {
		o.hooks.afterOpen()
	}

	// Lock 1 first, and nothing is read through the connection until the binding
	// to the locked file is verified: the first read of a swapped-in file could
	// run that file's hot-journal recovery. (A lock held by another process is
	// the one exception: the holder check reads the runner row to decide between
	// exit 3 and retrying, and then this runner exits or keeps waiting.)
	deadline := o.Now().Add(o.LockWait)
	dbLock, err := acquireDBLock(ctx, store, info.Canonical, deadline, o)
	if err != nil {
		return err
	}
	defer dbLock.Close()
	if o.hooks.afterLocks != nil {
		o.hooks.afterLocks()
	}
	if err := verifyBinding(info, dbLock, fdsBefore, o); err != nil {
		return err
	}
	// Only now is the connection known to be on the locked file, so only now
	// may it read or change the file.
	if err := d.EnableWAL(); err != nil {
		return exitf(ExitRefused, "enable WAL on %s: %w", info.Canonical, err)
	}
	if err := d.CheckSchemaVersion(); err != nil {
		return exitf(ExitRefused, "%s: %w", info.Canonical, err)
	}
	row, err := store.ReadRunner(ctx)
	if err != nil {
		return exitf(ExitRefused, "%w", err)
	}
	queueLock, err := acquireQueueLock(ctx, store, info.Canonical, row.QueueID, deadline, o)
	if err != nil {
		return err
	}
	defer queueLock.Close()

	// Re-read after lock 2: the row may have changed while we waited.
	row, err = store.ReadRunner(ctx)
	if err != nil {
		return exitf(ExitRefused, "%w", err)
	}

	if cg != nil {
		cg.setQueue(row.QueueID)
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
	return newEngine(o, store, d.SqlDB(), epoch, cg).run(ctx)
}

// verifyBinding makes the preflight verdict, the lock and the recorded
// identity refer to one file. The path is resolved separately by Preflight,
// by SQLite and by LockDB, so a swap between them could leave the lock on one
// inode and the connection on another, or skip the live-DB refusal. The lock
// descriptor must be the inode Preflight checked, and a second Preflight now
// must resolve to the same canonical path and inode. A swap after this check
// cannot matter: SQLite and the lock hold descriptors on the original inode.
func verifyBinding(info PathInfo, lock *os.File, fdsBefore map[string]fdID, o Options) error {
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
	if err := sqliteOnLockedFile(info, lock, fdsBefore); err != nil {
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

// acquireDBLock takes lock 1 (flock on the DB inode). When it is held and the
// recorded holder is verified alive, it exits 3 at once. Otherwise the holder
// may be exiting, so it retries until the deadline and exits 1.
func acquireDBLock(ctx context.Context, store *scheduler.Store, canonical string, deadline time.Time, o Options) (*os.File, error) {
	for {
		f, lerr := LockDB(canonical)
		switch {
		case lerr == nil:
			return f, nil
		case errors.Is(lerr, ErrHeld):
			if err := holderCheck(ctx, store, canonical); err != nil {
				return nil, err
			}
		default:
			return nil, &ExitError{Code: ExitRefused, Err: lerr}
		}
		if !o.Now().Before(deadline) {
			return nil, exitf(ExitRefused, "database lock on %s held by another process, and no live runner is recorded", canonical)
		}
		select {
		case <-ctx.Done():
			return nil, &ExitError{Code: ExitRefused, Err: ctx.Err()}
		case <-time.After(o.RetryEvery):
		}
	}
}

// acquireQueueLock takes lock 2 (the queue's abstract socket), with the same
// holder check and retry as lock 1.
func acquireQueueLock(ctx context.Context, store *scheduler.Store, canonical, queueID string, deadline time.Time, o Options) (interface{ Close() error }, error) {
	for {
		ln, lerr := LockQueue(queueID)
		switch {
		case lerr == nil:
			return ln, nil
		case errors.Is(lerr, ErrHeld):
			if err := holderCheck(ctx, store, canonical); err != nil {
				return nil, err
			}
		default:
			return nil, &ExitError{Code: ExitRefused, Err: lerr}
		}
		if !o.Now().Before(deadline) {
			return nil, exitf(ExitRefused, "queue lock held by another process (no live runner is recorded; a squatter holds the abstract socket, see ss -xlp)")
		}
		select {
		case <-ctx.Done():
			return nil, &ExitError{Code: ExitRefused, Err: ctx.Err()}
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

type fdID struct {
	dev, ino uint64
	reg      bool   // a regular file
	target   string // what /proc/self/fd/N points at
}

// snapshotFDs records what every open descriptor of this process refers to.
func snapshotFDs() (map[string]fdID, error) {
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return nil, err
	}
	m := make(map[string]fdID, len(ents))
	for _, e := range ents {
		var st syscall.Stat_t
		if err := syscall.Stat("/proc/self/fd/"+e.Name(), &st); err != nil {
			continue // the directory handle used for this listing, or a socket
		}
		target, _ := os.Readlink("/proc/self/fd/" + e.Name())
		m[e.Name()] = fdID{uint64(st.Dev), uint64(st.Ino), st.Mode&syscall.S_IFMT == syscall.S_IFREG, target}
	}
	return m, nil
}

// allowPreopenedFDs is a seam for tests that start several runners in one
// process, where an earlier connection's descriptor is still open on the
// database. A real runner is a fresh process, so it is false there.
var allowPreopenedFDs = false

// refusePreopened refuses to start when this process already holds a
// descriptor on the database file. Such a descriptor (inherited, or left
// pending close by an earlier SQLite connection) cannot be told apart from the
// one SQLite opens for this runner, so it could make the binding check pass
// while SQLite acts on another file.
func refusePreopened(info PathInfo, before map[string]fdID) error {
	if allowPreopenedFDs {
		return nil
	}
	want := fdID{dev: info.Dev, ino: info.Ino}
	for name, id := range before {
		if id.reg && id.sameFile(want) {
			return exitf(ExitRefused, "refusing to start: this process already has %s open (descriptor %s, %s); a runner needs a fresh process",
				info.Canonical, name, id.target)
		}
	}
	return nil
}

func (a fdID) sameFile(b fdID) bool { return a.dev == b.dev && a.ino == b.ino }

// sqliteOnLockedFile checks that the SQLite connection holds the locked inode.
// SQLite resolves the path on its own, so it can land on a different file than
// the one preflight checked and the lock holds (a rename or symlink swap between
// those steps); the lock-descriptor and path checks cannot see that. The
// descriptors that SQLite opened are the ones that appeared after the snapshot
// taken just before the open, so a descriptor that was already open on the
// locked inode (inherited, or from any other part of the process) cannot stand
// in for a new one. The lock descriptor is excluded by number. The caller's
// reads have forced the single connection open before this runs.
//
// With allowPreopenedFDs (tests only) one case has no new descriptor on the
// locked inode: SQLite reuses a descriptor that an earlier connection to the
// same file left pending close in the process. It does so only when stat() of
// the path matches that descriptor's inode, and then it never resolves the path
// again. That case is accepted only when SQLite opened nothing on any other
// regular file except the sidecar files of the canonical path. A real runner
// never gets here with a descriptor already open on the file (refusePreopened),
// so for it a new descriptor on the locked inode is required.
//
// The first read through the connection happens before the binding is verified
// (the queue id needed for lock 2 lives in the file). On a swapped-in database
// with a hot journal that read completes the crash recovery that the file's
// next opener would perform; it adds no logical data and rewrites no committed
// state. Nothing else is written before the check passes.
//
// A connection that the pool later reopens would resolve the path again. That
// cannot let this runner act on another database: every write it makes is
// guarded by its epoch, which no other file carries.
func sqliteOnLockedFile(info PathInfo, lock *os.File, before map[string]fdID) error {
	after, err := snapshotFDs()
	if err != nil {
		return exitf(ExitRefused, "cannot inspect open descriptors to bind SQLite to the locked database: %w", err)
	}
	own := strconv.FormatUint(uint64(lock.Fd()), 10)
	want := fdID{dev: info.Dev, ino: info.Ino}
	reusable := false
	for name, id := range after {
		if name == own {
			continue
		}
		prev, existed := before[name]
		fresh := !existed || !prev.sameFile(id) // a rename changes the target, not the file
		switch {
		case id.sameFile(want) && fresh:
			return nil
		case id.sameFile(want):
			reusable = true
		case fresh && id.reg && !isSidecar(id.target, info.Canonical):
			return exitf(ExitRefused, "database file changed during startup: SQLite opened %s, not the locked file %s (%s)",
				id.target, info.Canonical, info.Identity())
		}
	}
	if reusable && allowPreopenedFDs {
		return nil
	}
	return exitf(ExitRefused, "database file changed during startup: SQLite did not open the locked file %s (%s)", info.Canonical, info.Identity())
}

func isSidecar(target, canonical string) bool {
	for _, suf := range []string{"-wal", "-shm", "-journal"} {
		if target == canonical+suf {
			return true
		}
	}
	return false
}
