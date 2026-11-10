//go:build !windows

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// reapChild collects a child the test process spawned, standing in for the init
// process that adopts a detached agent once `ic dispatch spawn` has exited. A
// zombie still answers kill(pid, 0), so liveness proofs must reap it first.
func reapChild(pid int) {
	var status syscall.WaitStatus
	_, _ = syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
}

func waitGone(t *testing.T, what string, pid int, reap bool, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if reap {
			reapChild(pid)
		}
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s (pid %d) is still alive %s after rollback", what, pid, within)
}

func readPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 1 {
				return pid
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("pid file %s was never written", path)
	return 0
}

// TestRunRollbackStopsDispatchProcessGroup proves that `ic run rollback` ends
// the agent process and the children it started, not only the dispatch row.
func TestRunRollbackStopsDispatchProcessGroup(t *testing.T) {
	setupCommandMetadataDB(t)
	ctx := context.Background()

	out := captureDispatchOutput(t, func() int {
		return cmdRunCreate(ctx, []string{"--goal=rollback stops agents", "--project=."})
	})
	runID := strings.TrimSpace(string(out))
	for range 3 {
		captureDispatchOutput(t, func() int { return cmdRunAdvance(ctx, []string{runID, "--disable-gates"}) })
	}

	// A fake agent: a group leader that starts a long-lived child.
	dir := t.TempDir()
	prompt := filepath.Join(dir, "prompt.md")
	wrapper := filepath.Join(dir, "dispatch.sh")
	leaderPID, childPID := filepath.Join(dir, "leader.pid"), filepath.Join(dir, "child.pid")
	script := "#!/bin/sh\nsleep 300 &\necho $! > " + childPID + "\necho $$ > " + leaderPID + "\nwait\n"
	for path, content := range map[string]string{prompt: "fixture", wrapper: script} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	flagJSON = true
	spawnOut := captureDispatchOutput(t, func() int {
		return cmdDispatchSpawn(ctx, []string{"--prompt-file=" + prompt, "--dispatch-sh=" + wrapper, "--run-id=" + runID, "--project=" + dir})
	})
	var spawned struct {
		ID  string `json:"id"`
		PID int    `json:"pid"`
	}
	if err := json.Unmarshal(spawnOut, &spawned); err != nil {
		t.Fatalf("spawn output %q: %v", spawnOut, err)
	}
	leader, child := readPIDFile(t, leaderPID), readPIDFile(t, childPID)
	t.Cleanup(func() {
		_ = syscall.Kill(-leader, syscall.SIGKILL)
		_ = syscall.Kill(child, syscall.SIGKILL)
		reapChild(leader)
	})
	if leader != spawned.PID {
		t.Fatalf("fake agent pid %d, spawn reported %d", leader, spawned.PID)
	}
	if err := syscall.Kill(leader, 0); err != nil {
		t.Fatalf("agent is not running before rollback: %v", err)
	}

	// Stand in for init: the real CLI process that spawned the agent has exited.
	stopReaping := make(chan struct{})
	go func() {
		for {
			select {
			case <-stopReaping:
				return
			default:
				reapChild(leader)
				time.Sleep(25 * time.Millisecond)
			}
		}
	}()
	defer close(stopReaping)

	flagJSON = false
	captureDispatchOutput(t, func() int {
		return cmdRunRollback(ctx, []string{runID, "--to-phase=brainstorm", "--reason=test"})
	})

	waitGone(t, "agent process", leader, false, 15*time.Second)
	waitGone(t, "agent's child process", child, false, 15*time.Second)

	d, err := openDB()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var status string
	if err := d.SqlDB().QueryRow("SELECT status FROM dispatches WHERE id = ?", spawned.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "cancelled" {
		t.Fatalf("dispatch status after rollback = %q, want cancelled", status)
	}
}
