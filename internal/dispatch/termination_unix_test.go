//go:build !windows

package dispatch

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestReviewV2KillRefusesUnownedProcessGroup(t *testing.T) {
	for _, receipt := range []bool{false, true} {
		t.Run(strconv.FormatBool(receipt), func(t *testing.T) {
			cmd := exec.Command("sleep", "60")
			cmd.SysProcAttr = platformSysProcAttr()
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			go cmd.Wait()
			t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
			store := testStore(t)
			id, err := store.Create(context.Background(), &Dispatch{AgentType: "flere", ProjectDir: "."})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.UpdateStatus(context.Background(), id, StatusRunning, UpdateFields{"pid": cmd.Process.Pid}); err != nil {
				t.Fatal(err)
			}
			if receipt {
				// An unversioned legacy record is not comparable with the new stable
				// birth format; upgrading metadata must not impersonate a PID reuse.
				payload, _ := json.Marshal(map[string]any{"pid": cmd.Process.Pid, "group_id": cmd.Process.Pid, "birth": "previous-process"})
				if _, err := store.db.Exec(`INSERT INTO state(key,scope_id,payload) VALUES ('dispatch.process',?,?)`, id, string(payload)); err != nil {
					t.Fatal(err)
				}
			}
			err = Kill(context.Background(), store, id)
			if !isProcessAlive(cmd.Process.Pid) {
				t.Fatal("unowned group was signalled")
			}
			requireRejection(t, err, "process_identity_unverified")
			d, _ := store.Get(context.Background(), id)
			if d.Status != StatusFailed || d.QuarantineReason == nil || *d.QuarantineReason != WorkerOutcomeIndeterminate {
				t.Fatalf("unknown outcome not preserved: %+v", d)
			}
			if ShouldRetry(d, DefaultRetryPolicy()) {
				t.Fatal("unowned outcome was retryable")
			}
		})
	}
}

func TestReviewV2KillCollectsExitedWorkerReceipt(t *testing.T) {
	store := testStore(t)
	id, output, receipt := workerReceiptFixture(t, store)
	saveWorkerReceipt(t, output, receipt)
	if err := store.UpdateStatus(context.Background(), id, StatusRunning, UpdateFields{"pid": 999999999}); err != nil {
		t.Fatal(err)
	}
	if err := Kill(context.Background(), store, id); err != nil {
		t.Fatal(err)
	}
	d, err := store.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != StatusCompleted || d.InputTokens != 13 {
		t.Fatalf("verified exited attempt was discarded: %+v", d)
	}
}

func TestReviewKillTerminatesOwnedProcessGroup(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "child.pid")
	cmd := exec.Command("sh", "-c", `trap '' TERM; sleep 60 & echo $! > "$1"; wait`, "fixture", marker)
	cmd.SysProcAttr = platformSysProcAttr()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	deadline := time.Now().Add(time.Second)
	var child int
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(marker)
		child, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		if child > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if child == 0 {
		t.Fatal("no child marker")
	}
	identity, err := readProcessIdentity(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := terminateRecordedProcess(identity); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("leader survived termination")
	}
	deadline = time.Now().Add(2 * time.Second)
	for isProcessAlive(child) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if isProcessAlive(child) {
		_ = syscall.Kill(child, syscall.SIGKILL)
		t.Fatal("descendant survived leader termination")
	}
}

func TestReviewFlereKillAndTimeoutAreIndeterminateFailures(t *testing.T) {
	for _, mode := range []string{"kill", "timeout", "no-pid"} {
		t.Run(mode, func(t *testing.T) {
			store := testStore(t)
			d := &Dispatch{AgentType: "flere", ProjectDir: "."}
			if mode != "no-pid" {
				cmd := exec.Command("sleep", "60")
				cmd.SysProcAttr = platformSysProcAttr()
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				go cmd.Wait()
				t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
				pid := cmd.Process.Pid
				d.PID = &pid
			}
			id, err := store.Create(context.Background(), d)
			if err != nil {
				t.Fatal(err)
			}
			if d.PID != nil {
				if _, err := store.recordProcessIdentity(context.Background(), id, *d.PID); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "timeout" {
				_, err = Wait(context.Background(), store, id, time.Second, 50*time.Millisecond)
			} else {
				err = Kill(context.Background(), store, id)
			}
			if err != nil {
				t.Fatal(err)
			}
			d, err = store.Get(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if d.Status != StatusFailed || d.QuarantineReason == nil || *d.QuarantineReason != WorkerOutcomeIndeterminate {
				t.Fatalf("unknown worker outcome unclassified: %+v", d)
			}
		})
	}
}

func TestReviewV2TerminationUsesSurvivingGroupWitness(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "child.pid")
	cmd := exec.Command("sh", "-c", `sh -c 'trap "" TERM; echo $$ > "$1"; sleep 60' child "$1" & wait`, "fixture", marker)
	cmd.SysProcAttr = platformSysProcAttr()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go cmd.Wait()
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	var child int
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(marker)
		child, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		if child > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if child == 0 {
		t.Fatal("child was not ready")
	}
	identity, err := readProcessIdentity(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := terminateRecordedProcess(identity); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for isProcessAlive(child) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if isProcessAlive(child) {
		t.Fatal("known descendant survived its leader")
	}
}
