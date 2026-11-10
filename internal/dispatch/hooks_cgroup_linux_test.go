//go:build linux

package dispatch

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// Unit test: CgroupFD reaches the exec attributes and keeps the process group.
func TestCgroupFDSetsSysProcAttr(t *testing.T) {
	opts := SpawnOptions{AgentType: "codex", ProjectDir: t.TempDir(), PromptFile: "p", DispatchSH: "/bin/true", CgroupFD: 7}
	cmd, err := buildCmd(opts, "out")
	if err != nil {
		t.Fatal(err)
	}
	a := cmd.SysProcAttr
	if !a.UseCgroupFD || a.CgroupFD != 7 {
		t.Fatalf("SysProcAttr = UseCgroupFD %v CgroupFD %d; want true, 7", a.UseCgroupFD, a.CgroupFD)
	}
	if !a.Setpgid {
		t.Fatal("CgroupFD dropped Setpgid")
	}
	opts.CgroupFD = 0
	cmd, err = buildCmd(opts, "out")
	if err != nil {
		t.Fatal(err)
	}
	if cmd.SysProcAttr.UseCgroupFD || cmd.SysProcAttr.CgroupFD != 0 {
		t.Fatal("CgroupFD unset must leave exec attributes untouched")
	}
}

func TestCgroupFDRejectsNegative(t *testing.T) {
	opts := SpawnOptions{ProjectDir: "/p", PromptFile: "/q", CgroupFD: -1}
	var in *InputError
	if err := opts.Validate(); !errors.As(err, &in) {
		t.Fatalf("Validate = %v, want an InputError", err)
	}
}

// With a writable cgroup v2 subtree (a delegated user scope), the worker
// really starts inside the given cgroup. Skipped otherwise.
func TestCgroupFDStartsWorkerInCgroup(t *testing.T) {
	self, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		t.Skip("no /proc/self/cgroup")
	}
	rel := strings.TrimSpace(strings.TrimPrefix(strings.SplitN(strings.TrimSpace(string(self)), "\n", 2)[0], "0::"))
	child := filepath.Join("/sys/fs/cgroup", rel, fmt.Sprintf("ic-test-%d", os.Getpid()))
	if err := os.Mkdir(child, 0o755); err != nil {
		t.Skipf("no writable delegated cgroup v2 subtree: %v", err)
	}
	t.Cleanup(func() {
		_ = os.WriteFile(filepath.Join(child, "cgroup.kill"), []byte("1"), 0o644)
		_ = os.Remove(child)
	})
	fd, err := syscall.Open(child, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)

	store := testStore(t)
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "dispatch.sh")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\ncat /proc/self/cgroup > \"$(dirname \"$0\")/cg.out\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	prompt := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(prompt, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := spawnAndWait(t, store, SpawnOptions{AgentType: "codex", ProjectDir: dir, PromptFile: prompt, DispatchSH: wrapper, OutputFile: filepath.Join(dir, "o.md"), CgroupFD: fd})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	_ = r
	raw, err := os.ReadFile(filepath.Join(dir, "cg.out"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "/ic-test-" + fmt.Sprint(os.Getpid()); !strings.HasSuffix(strings.TrimSpace(string(raw)), want) {
		t.Fatalf("worker cgroup = %q, want suffix %q", strings.TrimSpace(string(raw)), want)
	}
}
