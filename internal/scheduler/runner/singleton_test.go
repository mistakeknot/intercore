//go:build icsched_test && linux

package runner

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/mistakeknot/intercore/internal/db"
	"github.com/mistakeknot/intercore/internal/scheduler"
)

const waitFor = 10 * time.Second

// newQueue makes a migrated database file in a temp dir and a HOME elsewhere,
// so nothing here can reach a live database.
func newQueue(t *testing.T) (path string, store *scheduler.Store, raw *sql.DB) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	path = filepath.Join(t.TempDir(), "q.db")
	d, err := db.Open(path, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return path, scheduler.NewStore(d.SqlDB()), d.SqlDB()
}

type running struct {
	cancel context.CancelFunc
	done   chan struct{} // closed when Run returns; err is then valid
	err    error
	epoch  int64
}

// start runs a runner in this process and returns once its epoch is bumped.
func start(t *testing.T, o Options) *running {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{cancel: cancel, done: make(chan struct{})}
	ready := make(chan int64, 1)
	o.OnReady = func(e int64) { ready <- e }
	go func() { r.err = Run(ctx, o); close(r.done) }()
	select {
	case r.epoch = <-ready:
	case <-r.done:
		cancel()
		t.Fatalf("runner exited before it was ready: %v", r.err)
	case <-time.After(waitFor):
		cancel()
		t.Fatal("runner not ready")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-r.done:
		case <-time.After(waitFor):
		}
	})
	return r
}

func exitCode(err error) int {
	var ee *ExitError
	if errors.As(err, &ee) {
		return ee.Code
	}
	return -1
}

// A fake clock that advances on every read, so each heartbeat writes a new value.
func tickingClock() func() time.Time {
	var n atomic.Int64
	return func() time.Time { return time.Unix(1_800_000_000+n.Add(1), 0) }
}

// waitHeartbeat blocks until heartbeat_at moves past its current value, which
// proves the loop has run at least one more iteration.
func waitHeartbeat(t *testing.T, store *scheduler.Store) {
	t.Helper()
	ctx := context.Background()
	row, err := store.ReadRunner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	first := row.HeartbeatAt
	deadline := time.Now().Add(waitFor)
	for {
		row, _ = store.ReadRunner(ctx)
		if row.HeartbeatAt > first {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("heartbeat_at never advanced")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRunBumpsEpochHeartbeatsAndDrains(t *testing.T) {
	path, store, _ := newQueue(t)
	r := start(t, Options{DB: path, HeartbeatEvery: 5 * time.Millisecond, Now: tickingClock()})
	ctx := context.Background()
	row, err := store.ReadRunner(ctx)
	if err != nil || row.Epoch != r.epoch || row.State != "running" || row.PID != os.Getpid() || row.DBPath == "" || row.DBIdentity == "" || row.NetNS == 0 {
		t.Fatalf("row %+v %v", row, err)
	}
	first := row.HeartbeatAt
	deadline := time.Now().Add(waitFor)
	for {
		row, _ = store.ReadRunner(ctx)
		if row.HeartbeatAt > first {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("heartbeat_at never advanced")
		}
		time.Sleep(time.Millisecond)
	}
	r.cancel()
	<-r.done
	if r.err != nil {
		t.Fatalf("clean drain returned %v", r.err)
	}
	row, _ = store.ReadRunner(ctx)
	if row.State != "stopped" {
		t.Fatalf("state after drain %q", row.State)
	}
	if n, _ := store.InFlightCount(ctx, r.epoch); n != 0 {
		t.Fatalf("the skeleton claimed %d rows", n)
	}
}

func TestRunClaimsNothing(t *testing.T) {
	path, store, _ := newQueue(t)
	ctx := context.Background()
	if err := store.Create(ctx, &scheduler.SpawnJob{
		ID: "j1", Status: scheduler.StatusPending, Priority: scheduler.PriorityNormal, AgentType: "codex",
		SpawnOpts: "{}", MaxRetries: 3, CreatedAt: time.Unix(1_700_000_000, 0),
	}); err != nil {
		t.Fatal(err)
	}
	r := start(t, Options{DB: path, HeartbeatEvery: time.Millisecond})
	waitHeartbeat(t, store) // the loop has run; the job is still untouched
	r.cancel()
	<-r.done
	j, err := store.Get(ctx, "j1")
	if err != nil || j.Status != scheduler.StatusPending {
		t.Fatalf("job %+v %v", j, err)
	}
}

// A superseded runner exits 3. The control: before the epoch moves, it keeps running.
func TestRunSupersededExits3(t *testing.T) {
	path, store, raw := newQueue(t)
	r := start(t, Options{DB: path, HeartbeatEvery: 2 * time.Millisecond, Now: tickingClock()})
	waitHeartbeat(t, store)
	select {
	case <-r.done:
		t.Fatalf("control failed: runner exited while still current: %v", r.err)
	default:
	}
	if _, err := raw.ExecContext(context.Background(), `UPDATE scheduler_runner SET epoch = epoch + 1 WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	select {
	case <-r.done:
		if exitCode(r.err) != ExitSuperseded {
			t.Fatalf("got %v", r.err)
		}
	case <-time.After(waitFor):
		t.Fatal("a superseded runner kept running")
	}
}

// I7 / A4 in process: a second runner on the same DB exits 3 naming the holder.
func TestSecondRunnerExits3NamingHolder(t *testing.T) {
	path, store, _ := newQueue(t)
	r := start(t, Options{DB: path, HeartbeatEvery: time.Hour})
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(filepath.Dir(path), alias); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, filepath.Join(alias, "q.db")} {
		err := Run(context.Background(), Options{DB: p, LockWait: time.Second, RetryEvery: time.Millisecond})
		if exitCode(err) != ExitSuperseded || !strings.Contains(err.Error(), "pid "+itoa(os.Getpid())) {
			t.Fatalf("second runner via %s: %v", p, err)
		}
	}
	row, _ := store.ReadRunner(context.Background())
	if row.Epoch != r.epoch {
		t.Fatalf("a refused runner bumped the epoch to %d", row.Epoch)
	}
}

// A4b: a squatter holds the queue-lock name; the runner exits 1 naming it and bumps nothing.
func TestSquatterExits1(t *testing.T) {
	path, store, _ := newQueue(t)
	row, _ := store.ReadRunner(context.Background())
	sq, err := LockQueue(row.QueueID)
	if err != nil {
		t.Fatal(err)
	}
	defer sq.Close()
	err = Run(context.Background(), Options{DB: path, LockWait: 50 * time.Millisecond, RetryEvery: 5 * time.Millisecond})
	if exitCode(err) != ExitRefused || !strings.Contains(err.Error(), "queue lock held by another process") {
		t.Fatalf("got %v", err)
	}
	after, _ := store.ReadRunner(context.Background())
	if after.Epoch != row.Epoch {
		t.Fatal("epoch bumped despite the refusal")
	}
	// The failed runner must have let go of lock 1.
	f, err := LockDB(path)
	if err != nil {
		t.Fatalf("lock 1 leaked by a refused runner: %v", err)
	}
	f.Close()
}

// A1: a hardlinked DB is refused before anything is opened or bumped.
func TestHardlinkedDBRefused(t *testing.T) {
	path, store, _ := newQueue(t)
	if err := os.Link(path, filepath.Join(filepath.Dir(path), "other.db")); err != nil {
		t.Fatal(err)
	}
	before, _ := store.ReadRunner(context.Background())
	err := Run(context.Background(), Options{DB: path, LockWait: 20 * time.Millisecond})
	if exitCode(err) != ExitRefused || !errors.Is(err, ErrRefused) {
		t.Fatalf("got %v", err)
	}
	after, _ := store.ReadRunner(context.Background())
	if after.Epoch != before.Epoch {
		t.Fatal("epoch bumped")
	}
}

// A3: identity mismatch is refused; --rebind-db with confirmation succeeds, pauses, quarantines.
func TestRebindFlow(t *testing.T) {
	path, store, raw := newQueue(t)
	ctx := context.Background()
	r := start(t, Options{DB: path, HeartbeatEvery: time.Hour})
	r.cancel()
	<-r.done
	// Make the recorded identity belong to another database.
	if _, err := raw.ExecContext(ctx, `UPDATE scheduler_runner SET db_identity = '1:2', boot_id = 'dead-boot' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	err := Run(ctx, Options{DB: path, LockWait: 20 * time.Millisecond})
	if exitCode(err) != ExitRefused || !errors.Is(err, scheduler.ErrIdentityMismatch) {
		t.Fatalf("without --rebind-db: %v", err)
	}
	err = Run(ctx, Options{DB: path, Rebind: true, LockWait: 20 * time.Millisecond})
	if exitCode(err) != ExitRefused || !strings.Contains(err.Error(), "--confirm-old-runner-dead") {
		t.Fatalf("rebind without confirmation: %v", err)
	}
	row, _ := store.ReadRunner(ctx)
	if row.DBIdentity != "1:2" {
		t.Fatal("a refused rebind rewrote the identity")
	}
	r2 := start(t, Options{DB: path, Rebind: true, ConfirmOldRunnerDead: true, HeartbeatEvery: time.Hour})
	row, _ = store.ReadRunner(ctx)
	if row.DBIdentity == "1:2" || row.Epoch != r2.epoch {
		t.Fatalf("row after rebind %+v", row)
	}
	if paused, _ := store.IsPaused(ctx); !paused {
		t.Fatal("rebind did not pause the scheduler")
	}
}

// Rebind refuses while the recorded runner is present, even when the locks were free
// (a stopped process holds no flock when its fds are closed, but may resume).
func TestRebindRefusesPresentRunner(t *testing.T) {
	path, _, raw := newQueue(t)
	ctx := context.Background()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	stat, err := ReadStat(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	_, st, _ := ParseStat(stat)
	if _, err := raw.ExecContext(ctx,
		`UPDATE scheduler_runner SET pid = ?, pid_starttime = ?, boot_id = ?, db_identity = '1:2', db_path = ? WHERE id = 1`,
		cmd.Process.Pid, st, BootID(), path); err != nil {
		t.Fatal(err)
	}
	syscall.Kill(cmd.Process.Pid, syscall.SIGSTOP) // a stopped process counts as present
	err = Run(ctx, Options{DB: path, Rebind: true, ConfirmOldRunnerDead: true, LockWait: 20 * time.Millisecond})
	if exitCode(err) != ExitRefused || !strings.Contains(err.Error(), "still present") {
		t.Fatalf("got %v", err)
	}
	// Control: once the process is gone, the same rebind goes through.
	cmd.Process.Kill()
	cmd.Wait()
	r := start(t, Options{DB: path, Rebind: true, ConfirmOldRunnerDead: true, HeartbeatEvery: time.Hour})
	if r.epoch == 0 {
		t.Fatal("no epoch")
	}
}

// I9: a submitter that holds the DB open and is SIGSTOP'd takes no lock the
// runner needs, so the runner still acquires both.
func TestStoppedSubmitterDoesNotBlockRunner(t *testing.T) {
	path, _, _ := newQueue(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperHoldDB$")
	cmd.Env = append(os.Environ(), "IC_RUNNER_HELPER_DB="+path)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "HELD") {
		t.Fatalf("helper: %q %v", line, err)
	}
	syscall.Kill(cmd.Process.Pid, syscall.SIGSTOP)
	// Control: the helper does hold the DB open.
	held := false
	for _, l := range fdLinks(t, cmd.Process.Pid) {
		if l == path {
			held = true
		}
	}
	if !held {
		t.Fatal("control failed: the helper holds no descriptor on the DB")
	}
	r := start(t, Options{DB: path, HeartbeatEvery: time.Hour})
	if r.epoch < 2 {
		t.Fatalf("epoch %d", r.epoch)
	}
	syscall.Kill(cmd.Process.Pid, syscall.SIGCONT)
}

// TestHelperHoldDB is the body of the helper process above, not a test.
func TestHelperHoldDB(t *testing.T) {
	path := os.Getenv("IC_RUNNER_HELPER_DB")
	if path == "" {
		t.Skip("helper process only")
	}
	d, err := db.Open(path, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	tx, err := d.SqlDB().Begin() // an open read transaction, like a submitter mid-submit
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM scheduler_runner`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	os.Stdout.WriteString("HELD\n")
	time.Sleep(time.Minute)
}

// P2: a SIGTERM drain that cannot write "stopped" must not report a clean exit,
// because the singleton row would still say "running".
func TestDrainFailureIsReported(t *testing.T) {
	path, store, raw := newQueue(t)
	r := start(t, Options{DB: path, HeartbeatEvery: time.Hour, BusyTimeout: 100 * time.Millisecond})
	ctx := context.Background()
	conn, err := raw.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	r.cancel()
	<-r.done
	conn.ExecContext(ctx, `ROLLBACK`)
	conn.Close() // the pool has one connection; the reads below need it back
	if exitCode(r.err) != ExitRefused {
		t.Fatalf("drain that could not write returned %v, want exit %d", r.err, ExitRefused)
	}
	row, err := store.ReadRunner(ctx)
	if err != nil || row.State != "running" {
		t.Fatalf("row %+v %v", row, err)
	}
}

// P2 control: with no blocker the same drain is clean and the row says stopped.
func TestDrainWithoutBlockerIsClean(t *testing.T) {
	path, store, _ := newQueue(t)
	r := start(t, Options{DB: path, HeartbeatEvery: time.Hour, BusyTimeout: 100 * time.Millisecond})
	r.cancel()
	<-r.done
	if r.err != nil {
		t.Fatalf("clean drain returned %v", r.err)
	}
	if row, _ := store.ReadRunner(context.Background()); row.State != "stopped" {
		t.Fatalf("state %q", row.State)
	}
}

// P2: a superseded runner has nothing to write and still exits cleanly.
func TestDrainWhenSupersededIsClean(t *testing.T) {
	path, _, raw := newQueue(t)
	r := start(t, Options{DB: path, HeartbeatEvery: time.Hour})
	if _, err := raw.Exec(`UPDATE scheduler_runner SET epoch = epoch + 1`); err != nil {
		t.Fatal(err)
	}
	r.cancel()
	<-r.done
	if r.err != nil {
		t.Fatalf("superseded drain returned %v", r.err)
	}
}

// replaceWithCopy swaps the file at path for a byte-for-byte copy with a new
// inode, after emptying the WAL so the copy is complete.
func replaceWithCopy(t *testing.T, raw *sql.DB, path string) {
	t.Helper()
	if _, err := raw.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(filepath.Dir(path), "copy.db")
	if _, err := raw.Exec(`VACUUM INTO ?`, tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

func epochAt(t *testing.T, path string) int64 {
	t.Helper()
	d, err := db.Open(path, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	row, err := scheduler.NewStore(d.SqlDB()).ReadRunner(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return row.Epoch
}

// P1: the file preflight checked, the file SQLite opened and the file the
// lock holds must be one inode. Each swap point must be refused with no write.
func TestSwapDuringStartupIsRefused(t *testing.T) {
	for _, point := range []string{"beforeOpen", "afterOpen", "afterLocks"} {
		t.Run(point, func(t *testing.T) {
			path, store, raw := newQueue(t)
			before, _ := store.ReadRunner(context.Background())
			var fired atomic.Int32
			swap := func() { fired.Add(1); replaceWithCopy(t, raw, path) }
			o := Options{DB: path, LockWait: 20 * time.Millisecond, HeartbeatEvery: time.Hour}
			switch point {
			case "beforeOpen":
				o.hooks.beforeOpen = swap
			case "afterOpen":
				o.hooks.afterOpen = swap
			case "afterLocks":
				o.hooks.afterLocks = swap
			}
			err := Run(context.Background(), o)
			if fired.Load() != 1 {
				t.Fatalf("hook fired %d times", fired.Load())
			}
			if exitCode(err) != ExitRefused || !strings.Contains(err.Error(), "changed during startup") {
				t.Fatalf("got %v", err)
			}
			if got := epochAt(t, path); got != before.Epoch {
				t.Fatalf("epoch of the replacement file is %d, was %d", got, before.Epoch)
			}
			if row, _ := store.ReadRunner(context.Background()); row.Epoch != before.Epoch {
				t.Fatalf("epoch of the original file is %d, was %d", row.Epoch, before.Epoch)
			}
		})
	}
}

// P1: swapping the database for a symlink into the live directory after the
// preflight must not let the runner act on the live file.
func TestSwapToSymlinkIntoLiveDirRefused(t *testing.T) {
	path, _, raw := newQueue(t)
	home := os.Getenv("HOME")
	liveDir := filepath.Join(home, ".clavain")
	if err := os.MkdirAll(liveDir, 0o700); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(liveDir, "intercore.db")
	if _, err := raw.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`VACUUM INTO ?`, live); err != nil {
		t.Fatal(err)
	}
	liveEpoch := epochAt(t, live)
	o := Options{DB: path, LockWait: 20 * time.Millisecond, HeartbeatEvery: time.Hour}
	o.hooks.afterOpen = func() {
		if err := os.Remove(path); err != nil {
			t.Error(err)
		}
		if err := os.Symlink(live, path); err != nil {
			t.Error(err)
		}
	}
	err := Run(context.Background(), o)
	if exitCode(err) != ExitRefused {
		t.Fatalf("got %v", err)
	}
	if got := epochAt(t, live); got != liveEpoch {
		t.Fatalf("the live-directory database was written: epoch %d, was %d", got, liveEpoch)
	}
}

// P1 control: the hooks run in an ordinary startup and nothing is refused.
func TestStartupHooksFireWithoutSwap(t *testing.T) {
	path, _, _ := newQueue(t)
	var order []string
	o := Options{DB: path, HeartbeatEvery: time.Hour}
	o.hooks.beforeOpen = func() { order = append(order, "beforeOpen") }
	o.hooks.afterOpen = func() { order = append(order, "afterOpen") }
	o.hooks.afterLocks = func() { order = append(order, "afterLocks") }
	r := start(t, o)
	if strings.Join(order, ",") != "beforeOpen,afterOpen,afterLocks" {
		t.Fatalf("hook order %v", order)
	}
	if r.epoch < 2 {
		t.Fatalf("epoch %d", r.epoch)
	}
}
