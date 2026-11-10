//go:build icsched_test && linux

package runner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/mistakeknot/intercore/internal/scheduler"
)

// Tests for the three notes carried from slice 3c: the heartbeat during
// extinction, the empty cgroup directory left by an aborted launch, and the
// kill goroutine that run did not join.

func (r *rig) heartbeatAt() int64 {
	r.t.Helper()
	return int64(r.count(`SELECT heartbeat_at FROM scheduler_runner WHERE id = 1`))
}

// TestHeartbeatContinuesDuringExtinction: the main loop is inside a long
// extinction, so only the keep-alive can refresh the runner's heartbeat. The
// extinction is held at the populated-0 wait, the clock is moved on, and
// heartbeat_at must follow it before the extinction is released. Without the
// keep-alive heartbeat_at stays at its pre-extinction value and the test times
// out.
func TestHeartbeatContinuesDuringExtinction(t *testing.T) {
	r, o := containRig(t)
	clk := &testClock{}
	o.Now = clk.Now
	o.HeartbeatEvery = 20 * time.Millisecond
	entered := make(chan struct{})
	release := make(chan struct{})
	var once atomic.Bool
	o.hooks.populatedDelay = func() time.Duration {
		if once.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
		return 0
	}
	r.job("h", "ok")
	run := start(t, o)
	select {
	case <-entered:
	case <-time.After(waitFor):
		t.Fatal("the extinction was never reached")
	}
	before := r.heartbeatAt()
	clk.Advance(1000 * time.Second)
	want := clk.Now().Unix()
	until(t, fmt.Sprintf("heartbeat_at to reach %d while the extinction is held (is %d)", want, r.heartbeatAt()),
		func() bool { return r.heartbeatAt() >= want })
	if r.get("h").IsTerminal() {
		t.Fatal("the job finished while its extinction was held: the hold proves nothing")
	}
	if before >= want {
		t.Fatalf("heartbeat_at %d was already past the target %d", before, want)
	}
	close(release)
	r.waitStatus("h", scheduler.StatusCompleted)
	run.stop(t)
}

// TestAbortedLaunchRemovesItsEmptyCgroup: the runner is stopped while launch is
// still retrying its first marker write. Nothing was started in the attempt's
// cgroup, so the directory is removed instead of being left behind.
func TestAbortedLaunchRemovesItsEmptyCgroup(t *testing.T) {
	r, o := containRig(t)
	o.DrainTimeout = 200 * time.Millisecond
	o.hooks.inject = func(kind string) error {
		if kind == "marker" {
			return errors.New("injected SQLITE_BUSY (marker)")
		}
		return nil
	}
	r.job("a", "ok")
	run := start(t, o)
	var dir string
	until(t, "the attempt's cgroup directory", func() bool {
		m, _ := filepath.Glob(filepath.Join(o.CgroupDir, "attempt-*"))
		if len(m) == 1 {
			dir = m[0]
		}
		return dir != ""
	})
	run.stop(t)
	until(t, "the aborted launch to remove "+dir, func() bool {
		_, err := os.Stat(dir)
		return errors.Is(err, os.ErrNotExist)
	})
	if n := r.runs(); n != 0 {
		t.Fatalf("executor ran %d time(s); nothing should have started", n)
	}
}

// TestTrackedGoroutinesAreJoinedAndRefusedWhenClosing: a goroutine started
// with track is waited for by the join run performs on its way out, and once
// the engine is closing no new one is started.
func TestTrackedGoroutinesAreJoinedAndRefusedWhenClosing(t *testing.T) {
	e := newEngine(Options{Stderr: &logBuf{}}, nil, nil, 0, nil)
	release := make(chan struct{})
	var finished atomic.Bool
	if !e.track(func() { <-release; finished.Store(true) }) {
		t.Fatal("track refused before closing")
	}
	e.mu.Lock()
	e.closing = true
	e.mu.Unlock()
	if e.track(func() { t.Error("a goroutine ran after closing") }) {
		t.Fatal("track accepted work after closing")
	}
	joined := make(chan struct{})
	go func() { e.reapers.Wait(); close(joined) }()
	select {
	case <-joined:
		t.Fatal("the join returned while a tracked goroutine was still running")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-joined:
	case <-time.After(waitFor):
		t.Fatal("the join did not return after the goroutine finished")
	}
	if !finished.Load() {
		t.Fatal("the tracked goroutine did not finish before the join returned")
	}
}

// TestCancelAtMarkRunningKillsTheStartedProcess: a cancel that lands between
// the launch marker and the running marker makes launch kill the process after
// the waiter exists. The job ends cancelled, nothing is left running, and the
// runner drains cleanly (the kill is joined).
func TestCancelAtMarkRunningKillsTheStartedProcess(t *testing.T) {
	r, o := containRig(t)
	var cancelled atomic.Bool
	o.hooks.afterLaunchPersist = func(token, result string) {
		if result == scheduler.LaunchStarted && cancelled.CompareAndSwap(false, true) {
			r.cancel(r.jobOfToken(token))
		}
	}
	r.job("c", "hang")
	run := start(t, o)
	r.waitStatus("c", scheduler.StatusCancelled)
	pid := r.pidOf("c")
	until(t, "the cancelled process to be gone", func() bool { return gone(pid) })
	run.stop(t)
	if !cancelled.Load() {
		t.Fatal("the cancel hook never ran")
	}
}

// TestShutdownAbandonsAKillWaitingOnAStubbornProcess: a cancel kill is in
// flight against an executor that ignores SIGTERM (the graceful wait is five
// seconds) when the runner is stopped. The kill is joined by run but
// abort-aware, so the shutdown returns promptly instead of holding for the
// escalation delay. The process is left alive for recovery, not terminalized.
func TestShutdownAbandonsAKillWaitingOnAStubbornProcess(t *testing.T) {
	r, o := containRig(t)
	o.DrainTimeout = 200 * time.Millisecond
	var cancelled atomic.Bool
	o.hooks.afterLaunchPersist = func(token, result string) {
		if result == scheduler.LaunchStarted && cancelled.CompareAndSwap(false, true) {
			r.cancel(r.jobOfToken(token))
		}
	}
	r.job("s", "stubborn")
	run := start(t, o)
	until(t, "the cancel hook", func() bool { return cancelled.Load() })
	pid := r.pidOf("s")
	// The survivor is the point of this test, so it empties its own cgroup
	// before the rig's no-survivor check runs (cleanups run last-in first).
	t.Cleanup(func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = syscall.Kill(pid, syscall.SIGKILL)
		dirs, _ := filepath.Glob(filepath.Join(o.CgroupDir, "attempt-*"))
		for _, d := range dirs {
			_ = os.WriteFile(filepath.Join(d, "cgroup.kill"), []byte("1"), 0o644)
			for i := 0; i < 500; i++ {
				if pop, _ := populated(d); !pop {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
	})
	// Let the kill send its SIGTERM and enter the graceful wait.
	time.Sleep(500 * time.Millisecond)
	begin := time.Now()
	run.stop(t)
	if took := time.Since(begin); took > 2500*time.Millisecond {
		t.Fatalf("shutdown held %v by an in-flight kill (graceful wait is 5s)", took)
	}
	if gone(pid) {
		t.Fatal("the stubborn process should survive an abandoned kill")
	}
	var st string
	if err := r.raw.QueryRow(`SELECT status FROM dispatches WHERE id = ?`, r.dispatchOf("s")).Scan(&st); err != nil {
		t.Fatal(err)
	}
	switch st {
	case "completed", "failed", "timeout", "cancelled":
		t.Fatalf("abandoned kill terminalized the dispatch as %q", st)
	}
}
