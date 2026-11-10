//go:build icsched_test && linux

package main

// Integration tests for `ic scheduler run` (plan section 9, tests I7, I9, A1,
// A2, A3, A4, A4b, A7). They run the real icsched_test binary, always with a
// throwaway HOME, a working directory under the test's temp dir and an
// explicit --db, and never against a live database. Waits are on process
// output and exit, never on elapsed time.

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mistakeknot/intercore/internal/db"
	"github.com/mistakeknot/intercore/internal/scheduler/runner"
)

const waitLimit = 30 * time.Second

type proc struct {
	cmd   *exec.Cmd
	ready chan struct{}
	done  chan struct{}
	mu    sync.Mutex
	buf   bytes.Buffer
	queue string
	exit  int
}

func (p *proc) stderr() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.buf.String()
}

// launch starts cmd and watches its stderr. ready closes at the runner's
// "epoch N, queue Q" line; done closes when the process has exited.
func launch(t *testing.T, cmd *exec.Cmd) *proc {
	t.Helper()
	p := &proc{cmd: cmd, ready: make(chan struct{}), done: make(chan struct{})}
	pipe, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		select {
		case <-p.done:
		default:
			cmd.Process.Kill()
			<-p.done
		}
	})
	go func() {
		sc := bufio.NewScanner(pipe)
		readyClosed := false
		for sc.Scan() {
			line := sc.Text()
			p.mu.Lock()
			p.buf.WriteString(line + "\n")
			p.mu.Unlock()
			if !readyClosed && strings.HasPrefix(line, "ic scheduler run: epoch ") {
				if _, rest, ok := strings.Cut(line, ", queue "); ok {
					p.queue, _, _ = strings.Cut(rest, ",")
				}
				readyClosed = true
				close(p.ready)
			}
		}
		err := cmd.Wait()
		p.exit = 0
		if ee, ok := err.(*exec.ExitError); ok {
			p.exit = ee.ExitCode()
		} else if err != nil {
			p.exit = -1
		}
		close(p.done)
	}()
	return p
}

func (p *proc) waitReady(t *testing.T) {
	t.Helper()
	select {
	case <-p.ready:
	case <-p.done:
		t.Fatalf("runner exited %d before it was ready: %s", p.exit, p.stderr())
	case <-time.After(waitLimit):
		t.Fatalf("runner not ready: %s", p.stderr())
	}
}

func (p *proc) waitExit(t *testing.T) int {
	t.Helper()
	select {
	case <-p.done:
		return p.exit
	case <-time.After(waitLimit):
		t.Fatalf("runner did not exit: %s", p.stderr())
		return -1
	}
}

// stop sends SIGTERM and returns the exit code.
func (p *proc) stop(t *testing.T) int {
	t.Helper()
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	return p.waitExit(t)
}

type env struct {
	t    *testing.T
	bin  string
	home string
	cwd  string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	// EvalSymlinks: t.TempDir may sit behind a symlink, and the runner works on
	// canonical paths.
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, bin: buildIC(t, "icsched_test"), home: t.TempDir(), cwd: cwd}
}

func (e *env) environ() []string {
	return []string{"HOME=" + e.home, "PATH=" + os.Getenv("PATH")}
}

// newDB creates and migrates a database under the working directory.
func (e *env) newDB(rel string) string {
	e.t.Helper()
	path := filepath.Join(e.cwd, rel)
	d, err := db.Open(path, 5*time.Second)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := d.Migrate(context.Background()); err != nil {
		e.t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		e.t.Fatal(err)
	}
	return path
}

func (e *env) runnerCmd(rel string, extra ...string) *exec.Cmd {
	args := append([]string{"--db=" + rel, "scheduler", "run", "--heartbeat-interval=50ms", "--no-containment"}, extra...)
	cmd := exec.Command(e.bin, args...)
	cmd.Dir = e.cwd
	cmd.Env = e.environ()
	return cmd
}

func (e *env) runner(rel string, extra ...string) *proc {
	return launch(e.t, e.runnerCmd(rel, extra...))
}

func (e *env) query(path, q string, dest ...any) {
	e.t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		e.t.Fatal(err)
	}
	defer raw.Close()
	if err := raw.QueryRow(q).Scan(dest...); err != nil {
		e.t.Fatalf("%s: %v", q, err)
	}
}

func (e *env) epoch(path string) int64 {
	var n int64
	e.query(path, `SELECT epoch FROM scheduler_runner WHERE id = 1`, &n)
	return n
}

func (e *env) noStrayFiles(path string) {
	e.t.Helper()
	matches, _ := filepath.Glob(path + "*")
	for _, m := range matches {
		if m != path && !strings.HasSuffix(m, "-wal") && !strings.HasSuffix(m, "-shm") {
			e.t.Errorf("unexpected file %s", m)
		}
	}
}

// userNS reports whether unprivileged user namespaces work, with the reason.
func userNS(t *testing.T, flags ...string) {
	t.Helper()
	args := append(flags, "true")
	if out, err := exec.Command("unshare", args...).CombinedOutput(); err != nil {
		t.Skipf("unprivileged user namespaces unavailable (unshare %s: %v: %s)", strings.Join(flags, " "), err, strings.TrimSpace(string(out)))
	}
}

// I7 and A4: a second runner on the same DB exits 3 naming the holder pid,
// also through a symlinked directory alias. It must not bump the epoch.
func TestCLISecondRunnerExits3(t *testing.T) {
	e := newEnv(t)
	real := filepath.Join(e.cwd, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(e.cwd, "alias")); err != nil {
		t.Fatal(err)
	}
	path := e.newDB("real/q.db")

	a := e.runner("real/q.db")
	a.waitReady(t)
	epoch := e.epoch(path)

	for _, rel := range []string{"real/q.db", "alias/q.db"} {
		b := e.runner(rel, "--lock-wait=5s")
		if code := b.waitExit(t); code != 3 {
			t.Fatalf("%s: second runner exit %d, want 3: %s", rel, code, b.stderr())
		}
		want := fmt.Sprintf("another runner (pid %d)", a.cmd.Process.Pid)
		if !strings.Contains(b.stderr(), want) {
			t.Fatalf("%s: stderr %q does not name the holder (%q)", rel, b.stderr(), want)
		}
	}
	if got := e.epoch(path); got != epoch {
		t.Fatalf("a refused runner moved the epoch %d -> %d", epoch, got)
	}
	// Control: A is still the live runner, and exits 0 on SIGTERM.
	select {
	case <-a.done:
		t.Fatalf("runner A died: %s", a.stderr())
	default:
	}
	if code := a.stop(t); code != 0 {
		t.Fatalf("runner A exit %d: %s", code, a.stderr())
	}
}

// A4b: a squatter on the abstract queue-lock name makes the runner exit 1
// naming the queue lock. Nothing is claimed and the epoch does not move.
func TestCLISquatterExits1(t *testing.T) {
	e := newEnv(t)
	path := e.newDB("q.db")

	first := e.runner("q.db")
	first.waitReady(t)
	queue := first.queue
	if queue == "" {
		t.Fatalf("no queue id in %q", first.stderr())
	}
	if code := first.stop(t); code != 0 {
		t.Fatalf("first runner exit %d: %s", code, first.stderr())
	}
	epoch := e.epoch(path)

	ln, err := net.Listen("unix", "@"+runner.QueueSocketName(queue))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	b := e.runner("q.db", "--lock-wait=300ms")
	if code := b.waitExit(t); code != 1 {
		t.Fatalf("runner exit %d, want 1: %s", code, b.stderr())
	}
	if !strings.Contains(b.stderr(), "queue lock held by another process") {
		t.Fatalf("stderr does not name the queue lock: %q", b.stderr())
	}
	if got := e.epoch(path); got != epoch {
		t.Fatalf("epoch moved %d -> %d", epoch, got)
	}

	// Control: the same runner starts once the squatter is gone.
	ln.Close()
	c := e.runner("q.db", "--lock-wait=5s")
	c.waitReady(t)
	if code := c.stop(t); code != 0 {
		t.Fatalf("control runner exit %d: %s", code, c.stderr())
	}
}

// A1: a database with a second hardlink is refused, and the epoch does not move.
func TestCLIHardlinkedDBRefused(t *testing.T) {
	e := newEnv(t)
	path := e.newDB("q.db")
	epoch := e.epoch(path)
	if err := os.Link(path, filepath.Join(e.cwd, "twin.db")); err != nil {
		t.Skipf("hardlinks unavailable here: %v", err)
	}
	r := e.runner("q.db")
	if code := r.waitExit(t); code != 1 {
		t.Fatalf("exit %d, want 1: %s", code, r.stderr())
	}
	if !strings.Contains(r.stderr(), "hard links") {
		t.Fatalf("stderr %q does not name the hardlink", r.stderr())
	}
	if got := e.epoch(path); got != epoch {
		t.Fatalf("epoch moved %d -> %d", epoch, got)
	}
	// Control: with the twin removed the runner starts.
	if err := os.Remove(filepath.Join(e.cwd, "twin.db")); err != nil {
		t.Fatal(err)
	}
	ok := e.runner("q.db")
	ok.waitReady(t)
	if code := ok.stop(t); code != 0 {
		t.Fatalf("control exit %d: %s", code, ok.stderr())
	}
}

// A3: a copied database (new inode) is refused without --rebind-db; with both
// flags it succeeds, bumps the epoch and leaves the scheduler paused.
func TestCLIRebind(t *testing.T) {
	e := newEnv(t)
	a := e.newDB("a.db")
	first := e.runner("a.db")
	first.waitReady(t)
	if code := first.stop(t); code != 0 {
		t.Fatalf("first runner exit %d: %s", code, first.stderr())
	}
	b := filepath.Join(e.cwd, "b.db")
	data, err := os.ReadFile(a)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, data, 0o600); err != nil {
		t.Fatal(err)
	}
	epoch := e.epoch(b)

	r := e.runner("b.db")
	if code := r.waitExit(t); code != 1 {
		t.Fatalf("plain start on a copied database: exit %d, want 1: %s", code, r.stderr())
	}
	if got := e.epoch(b); got != epoch {
		t.Fatalf("a refused start moved the epoch %d -> %d", epoch, got)
	}
	r = e.runner("b.db", "--rebind-db")
	if code := r.waitExit(t); code != 1 || !strings.Contains(r.stderr(), "--confirm-old-runner-dead") {
		t.Fatalf("--rebind-db alone: exit %d: %s", code, r.stderr())
	}
	r = e.runner("b.db", "--rebind-db", "--confirm-old-runner-dead")
	r.waitReady(t)
	if got := e.epoch(b); got != epoch+1 {
		t.Fatalf("rebind epoch %d, want %d", got, epoch+1)
	}
	var paused string
	e.query(b, `SELECT payload FROM state WHERE key = 'scheduler_paused' AND scope_id = 'global'`, &paused)
	if paused != "true" {
		t.Fatalf("scheduler_paused = %q after rebind", paused)
	}
	var dbPath string
	e.query(b, `SELECT db_path FROM scheduler_runner WHERE id = 1`, &dbPath)
	if dbPath != b {
		t.Fatalf("recorded db_path %q, want %q", dbPath, b)
	}
	if code := r.stop(t); code != 0 {
		t.Fatalf("rebound runner exit %d: %s", code, r.stderr())
	}
}

// helperHoldTx is a re-exec of the test binary: it opens the database, holds
// an open read transaction and waits to be killed. It is the "submitter".
func TestHelperHoldTx(t *testing.T) {
	path := os.Getenv("IC_RUNNER_HELPER_TX")
	if path == "" {
		t.Skip("helper process")
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := raw.Begin()
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM scheduler_runner`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(os.Stderr, "holding")
	select {}
}

// I9: a submitter SIGSTOP'd with an open transaction does not stop a runner
// from acquiring the locks (plan: probes take no lock).
func TestCLIStoppedSubmitterDoesNotBlockRunner(t *testing.T) {
	e := newEnv(t)
	path := e.newDB("q.db")

	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperHoldTx$")
	cmd.Env = append(e.environ(), "IC_RUNNER_HELPER_TX="+path)
	h := launch(t, cmd)
	// The helper has no "epoch" line; wait for its marker.
	deadline := time.After(waitLimit)
	for !strings.Contains(h.stderr(), "holding") {
		select {
		case <-h.done:
			t.Fatalf("helper exited: %s", h.stderr())
		case <-deadline:
			t.Fatalf("helper never started holding: %s", h.stderr())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := h.cmd.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.cmd.Process.Signal(syscall.SIGCONT) })
	// Control: the helper really holds an open descriptor on the database.
	if !holdsFD(t, h.cmd.Process.Pid, path) {
		t.Fatal("helper holds no descriptor on the database; the test would prove nothing")
	}

	r := e.runner("q.db", "--lock-wait=5s")
	r.waitReady(t)
	if code := r.stop(t); code != 0 {
		t.Fatalf("runner exit %d: %s", code, r.stderr())
	}
}

func holdsFD(t *testing.T, pid int, path string) bool {
	t.Helper()
	dir := "/proc/" + strconv.Itoa(pid) + "/fd"
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, en := range entries {
		if target, err := os.Readlink(filepath.Join(dir, en.Name())); err == nil && target == path {
			return true
		}
	}
	return false
}

// helperLockPath is a re-exec of the test binary that tries the database lock
// on a path and reports the result on stdout.
func TestHelperLockPath(t *testing.T) {
	path := os.Getenv("IC_RUNNER_HELPER_LOCK")
	if path == "" {
		t.Skip("helper process")
	}
	f, err := runner.LockDB(path)
	switch {
	case err == nil:
		fmt.Println("LOCKED")
		f.Close()
	case errors.Is(err, runner.ErrHeld):
		fmt.Println("HELD")
	default:
		fmt.Println("ERROR", err)
	}
}

// A2 (slice 3a form): a runner holds the database; inside `unshare -Urm` a
// file bind mount of the DB contends for the same inode lock, and a runner
// started on the bind mount is refused by the mount-point check without
// moving the epoch. The A2 steps that need the after_link_before_start barrier
// and an execve count move to slice 3c with the barrier hooks.
func TestCLIBindMountAlias(t *testing.T) {
	userNS(t, "-Urm")
	e := newEnv(t)
	path := e.newDB("q.db")
	a := e.runner("q.db")
	a.waitReady(t)
	epoch := e.epoch(path)

	alias := filepath.Join(e.cwd, "bound.db")
	if err := os.WriteFile(alias, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	helperScript := fmt.Sprintf(`mount --bind q.db bound.db && exec %s -test.run='^TestHelperLockPath$'`, os.Args[0])
	inNS := func(script string, env ...string) (string, error) {
		cmd := exec.Command("unshare", "-Urm", "sh", "-c", script)
		cmd.Dir = e.cwd
		cmd.Env = append(e.environ(), env...)
		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &out
		err := cmd.Run()
		return out.String(), err
	}
	if out, err := inNS(`mount --bind q.db bound.db && cat /proc/self/mountinfo | grep -c bound.db`); err != nil {
		t.Skipf("file bind mounts unavailable in a user namespace: %v: %s", err, out)
	}

	// The inode lock is held through the bind mount.
	out, err := inNS(helperScript, "IC_RUNNER_HELPER_LOCK="+alias)
	if err != nil {
		t.Fatalf("lock helper: %v: %s", err, out)
	}
	if !strings.Contains(out, "HELD") {
		t.Fatalf("lock through a bind mount was not held: %s", out)
	}

	// A runner on the bind mount is refused and does not bump the epoch.
	script := fmt.Sprintf(`mount --bind q.db bound.db && exec %s --db=bound.db scheduler run --lock-wait=300ms`, e.bin)
	out, err = inNS(script)
	ee, ok := err.(*exec.ExitError)
	if !ok || ee.ExitCode() == 0 {
		t.Fatalf("runner on a bind mount was not refused: %v: %s", err, out)
	}
	if ee.ExitCode() != 1 || !strings.Contains(out, "mount point") {
		t.Fatalf("runner on a bind mount: exit %d, want 1 naming the mount point: %s", ee.ExitCode(), out)
	}
	if got := e.epoch(path); got != epoch {
		t.Fatalf("epoch moved %d -> %d", epoch, got)
	}

	// Control: with A gone the same helper takes the lock.
	if code := a.stop(t); code != 0 {
		t.Fatalf("runner A exit %d: %s", code, a.stderr())
	}
	out, _ = inNS(helperScript, "IC_RUNNER_HELPER_LOCK="+alias)
	if !strings.Contains(out, "LOCKED") {
		t.Fatalf("control: lock was not free after A exited: %s", out)
	}
}

// A7: a runner in a different network namespace on the same database is
// refused (the abstract queue socket is per namespace, so the recorded net_ns
// must differ).
func TestCLIDifferentNetNSRefused(t *testing.T) {
	userNS(t, "-Un", "--map-user="+strconv.Itoa(os.Getuid()))
	e := newEnv(t)
	path := e.newDB("q.db")
	first := e.runner("q.db")
	first.waitReady(t)
	if code := first.stop(t); code != 0 {
		t.Fatalf("first runner exit %d: %s", code, first.stderr())
	}
	epoch := e.epoch(path)

	cmd := exec.Command("unshare", "-Un", "--map-user="+strconv.Itoa(os.Getuid()),
		e.bin, "--db=q.db", "scheduler", "run", "--lock-wait=300ms", "--no-containment")
	cmd.Dir = e.cwd
	cmd.Env = e.environ()
	b := launch(t, cmd)
	if code := b.waitExit(t); code != 1 {
		t.Fatalf("runner in another netns: exit %d, want 1: %s", code, b.stderr())
	}
	if !strings.Contains(b.stderr(), "different database identity") {
		t.Fatalf("stderr %q does not report the identity mismatch", b.stderr())
	}
	if got := e.epoch(path); got != epoch {
		t.Fatalf("epoch moved %d -> %d", epoch, got)
	}
	// Control: a runner in this namespace still starts.
	c := e.runner("q.db")
	c.waitReady(t)
	if code := c.stop(t); code != 0 {
		t.Fatalf("control exit %d: %s", code, c.stderr())
	}
}
