//go:build !windows

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mistakeknot/intercore/internal/dispatch"
	"github.com/mistakeknot/intercore/internal/scheduler"
)

func TestReviewSchedulerCancellationTerminatesDescendants(t *testing.T) {
	setupCommandMetadataDB(t)
	d, err := openDB()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	dir := t.TempDir()
	prompt, wrapper, marker := filepath.Join(dir, "prompt"), filepath.Join(dir, "wrapper"), filepath.Join(dir, "child.pid")
	if err := os.WriteFile(prompt, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\ntrap '' TERM\nsleep 60 &\necho $! > \"$IC_REVIEW_CHILD\"\nwait\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("IC_REVIEW_CHILD", marker)
	opts, err := scheduler.MarshalSpawnOpts(dispatch.SpawnOptions{AgentType: "codex", ProjectDir: dir, PromptFile: prompt, DispatchSH: wrapper})
	if err != nil {
		t.Fatal(err)
	}
	job := scheduler.NewSpawnJob("", scheduler.JobTypeDispatch, "review")
	job.AgentType = "codex"
	job.SpawnOpts = opts
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- scheduler.DispatchExecutor(dispatch.New(d.SqlDB(), nil))(ctx, job) }()
	var pid int
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(marker)
		pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		if pid > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("no child marker")
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel result: %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("executor did not stop")
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) == syscall.ESRCH {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("descendant survived executor cancellation")
}
