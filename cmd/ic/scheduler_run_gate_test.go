package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

// buildIC builds the ic binary once per tag set and returns its path. The
// binary lives outside the test's working directory so a run in a scratch
// directory can be checked for stray files afterwards.
var (
	buildMu    sync.Mutex
	buildCache = map[string]string{}
	buildRoot  string
)

func buildIC(t *testing.T, tags string) string {
	t.Helper()
	buildMu.Lock()
	defer buildMu.Unlock()
	if p, ok := buildCache[tags]; ok {
		return p
	}
	if buildRoot == "" {
		dir, err := os.MkdirTemp("", "icsched-bin.")
		if err != nil {
			t.Fatal(err)
		}
		buildRoot = dir
	}
	name := "ic-release"
	if tags != "" {
		name = "ic-" + tags
	}
	out := filepath.Join(buildRoot, name)
	args := []string{"build", "-o", out}
	if tags != "" {
		args = append(args, "-tags", tags)
	}
	args = append(args, ".")
	cmd := exec.Command("go", args...)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build %v: %v\n%s", args, err, b)
	}
	buildCache[tags] = out
	return out
}

// cleanupBuiltIC removes the binaries buildIC built; TestMain calls it.
func cleanupBuiltIC() {
	if buildRoot != "" {
		os.RemoveAll(buildRoot)
	}
}

func TestSchedulerRunReleaseBuildTouchesNothing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("scheduler runner is Unix-only")
	}
	bin := buildIC(t, "")

	home := t.TempDir()
	cwd := t.TempDir()
	// A database path that does not exist yet: a runner that opened it would
	// create it, its -wal and -shm files, and its parent directory.
	dbPath := filepath.Join(cwd, ".clavain", "intercore.db")

	for _, args := range [][]string{
		{"--db=" + dbPath, "scheduler", "run"},
		{"--db=" + dbPath, "scheduler", "run", "--rebind-db", "--confirm-old-runner-dead"},
		{"--db=" + dbPath, "scheduler", "run", "--no-containment", "--max-concurrent=1"},
	} {
		cmd := exec.Command(bin, args...)
		cmd.Dir = cwd
		cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		err := cmd.Run()
		ee, ok := err.(*exec.ExitError)
		if !ok || ee.ExitCode() != 2 {
			t.Fatalf("args %v: want exit 2, got %v (stderr %q)", args, err, stderr.String())
		}
		if got, want := stderr.String(), "ic: scheduler runner not enabled in this build\n"; got != want {
			t.Fatalf("args %v: stderr = %q, want %q", args, got, want)
		}
	}

	for _, dir := range []string{cwd, home} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			var names []string
			for _, e := range entries {
				names = append(names, e.Name())
			}
			t.Fatalf("release `scheduler run` left files in %s: %v", dir, names)
		}
	}
}

// The release binary carries none of the runner's test machinery: the barrier
// and fault hook environment variables exist only in files built under the
// icsched_test tag.
func TestReleaseBinaryHasNoRunnerTestHooks(t *testing.T) {
	bin := buildIC(t, "")
	data, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"IC_SCHED_BARRIER", "IC_SCHED_FAULT", "scheduler-runner-core"} {
		if bytes.Contains(data, []byte(marker)) {
			t.Errorf("release binary contains %q", marker)
		}
	}
}
