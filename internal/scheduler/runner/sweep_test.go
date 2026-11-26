//go:build icsched_test && linux

package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Slice 4d: the fallback-mode environment sweep, from procfs root fixtures and
// from the real procfs.

// envProc adds a process directory to a fixture root with the given state and
// environment entries (NUL separated, as /proc/<pid>/environ has them).
func envProc(t *testing.T, root string, pid int, state byte, env ...string) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(statText(pid, state, pid, 9000+uint64(pid))), 0o644); err != nil {
		t.Fatal(err)
	}
	var b []byte
	for _, kv := range env {
		b = append(b, kv...)
		b = append(b, 0)
	}
	if err := os.WriteFile(filepath.Join(dir, "environ"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func sweepEngine(root string) *engine {
	e := testEngine(root, "boot-A")
	e.o.Stderr = &logBuf{}
	return e
}

func TestSweepEnvironmentFixture(t *testing.T) {
	const id = "d1"
	cases := []struct {
		name  string
		build func(t *testing.T, root string)
		kill  bool
		want  sweepOutcome
		alive bool
		clean bool
	}{
		{"nothing running", func(t *testing.T, root string) {}, true, sweepOutcome{}, false, true},
		{"a candidate is found and, with no pidfd, not killed", func(t *testing.T, root string) {
			envProc(t, root, 5001, 'S', "A=1", "IC_DISPATCH_ID="+id)
		}, true, sweepOutcome{Found: 1}, true, false},
		{"look-only finds without killing", func(t *testing.T, root string) {
			envProc(t, root, 5001, 'S', "IC_DISPATCH_ID="+id)
		}, false, sweepOutcome{Found: 1}, true, false},
		{"several processes share the id", func(t *testing.T, root string) {
			envProc(t, root, 5001, 'S', "IC_DISPATCH_ID="+id)
			envProc(t, root, 5002, 'R', "IC_DISPATCH_ID="+id)
			envProc(t, root, 5003, 'S', "IC_DISPATCH_ID="+id)
		}, true, sweepOutcome{Found: 3}, true, false},
		{"another dispatch, a longer id, and a tail of another variable do not match", func(t *testing.T, root string) {
			envProc(t, root, 5001, 'S', "IC_DISPATCH_ID=other")
			envProc(t, root, 5002, 'S', "IC_DISPATCH_ID="+id+"x")
			envProc(t, root, 5003, 'S', "XIC_DISPATCH_ID="+id)
			envProc(t, root, 5004, 'S', "A=IC_DISPATCH_ID="+id)
		}, true, sweepOutcome{}, false, true},
		{"a zombie keeps no environment and is skipped", func(t *testing.T, root string) {
			envProc(t, root, 5001, 'Z', "IC_DISPATCH_ID="+id)
		}, true, sweepOutcome{}, false, true},
		{"an unreadable environment is unproven, not alive", func(t *testing.T, root string) {
			envProc(t, root, 5001, 'S', "IC_DISPATCH_ID="+id)
			// A directory where environ should be: the read fails with EISDIR as
			// a permission failure would, even for a privileged test user.
			dir := filepath.Join(root, "5001", "environ")
			if err := os.Remove(dir); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
		}, true, sweepOutcome{Unproven: 1}, false, false},
		{"a malformed stat is unproven", func(t *testing.T, root string) {
			envProc(t, root, 5001, 'S', "IC_DISPATCH_ID="+id)
			if err := os.WriteFile(filepath.Join(root, "5001", "stat"), []byte("5001 (x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, true, sweepOutcome{Unproven: 1}, false, false},
		{"non-numeric entries and pid 1 are skipped", func(t *testing.T, root string) {
			envProc(t, root, 1, 'S', "IC_DISPATCH_ID="+id)
			if err := os.Mkdir(filepath.Join(root, "self"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, true, sweepOutcome{}, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			tc.build(t, root)
			got := sweepEngine(root).sweepEnvironment(id, tc.kill)
			if got != tc.want {
				t.Fatalf("sweep = %+v, want %+v", got, tc.want)
			}
			if got.Alive() != tc.alive || got.Clean() != tc.clean {
				t.Fatalf("Alive=%v Clean=%v, want %v %v", got.Alive(), got.Clean(), tc.alive, tc.clean)
			}
		})
	}
}

func TestSweepEnvironmentUnlistableRootIsUnproven(t *testing.T) {
	got := sweepEngine(filepath.Join(t.TempDir(), "missing")).sweepEnvironment("d1", true)
	if got.Unproven != 1 || got.Clean() {
		t.Fatalf("sweep = %+v, want one unproven and not clean", got)
	}
}

// A process that is replaced between the reads is not the one that matched.
func TestSweepRecheckStartTimeChange(t *testing.T) {
	root := t.TempDir()
	envProc(t, root, 5001, 'S', "A=1")
	dirfd, err := unix.Open(filepath.Join(root, "5001"), unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(dirfd)
	if alive, bad := recheck(dirfd, 14001); !alive || bad {
		t.Fatalf("same start: alive=%v bad=%v", alive, bad)
	}
	if alive, bad := recheck(dirfd, 1234); alive || !bad {
		t.Fatalf("changed start: alive=%v bad=%v, want bad", alive, bad)
	}
	if err := os.WriteFile(filepath.Join(root, "5001", "stat"), []byte(statText(5001, 'Z', 5001, 14001)), 0o644); err != nil {
		t.Fatal(err)
	}
	if alive, bad := recheck(dirfd, 14001); alive || bad {
		t.Fatalf("zombie: alive=%v bad=%v, want gone", alive, bad)
	}
	if err := os.RemoveAll(filepath.Join(root, "5001")); err == nil {
		if alive, bad := recheck(dirfd, 14001); alive || bad {
			t.Fatalf("removed: alive=%v bad=%v, want gone", alive, bad)
		}
	}
}

func TestEnvironHas(t *testing.T) {
	env := []byte("A=1\x00IC_DISPATCH_ID=abc\x00B=2\x00")
	if !environHas(env, []byte("IC_DISPATCH_ID=abc")) {
		t.Fatal("exact entry not found")
	}
	if environHas(env, []byte("IC_DISPATCH_ID=ab")) || environHas(env, []byte("C_DISPATCH_ID=abc")) {
		t.Fatal("a prefix or suffix matched")
	}
}

// On the real procfs the sweep kills through a pidfd every same-uid process that
// carries the id, and only those: the one that cleared its environment is not
// found, which is why the caller never reads a clean sweep as proof of absence.
func TestSweepEnvironmentRealProcfsKills(t *testing.T) {
	id := "sweep-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	start := func(env ...string) int {
		cmd := exec.Command("sleep", "120")
		cmd.Env = env
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		go func() { _ = cmd.Wait() }()
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		return cmd.Process.Pid
	}
	a := start("IC_DISPATCH_ID=" + id)
	b := start("PATH=/usr/bin:/bin", "IC_DISPATCH_ID="+id)
	clear := start("PATH=/usr/bin:/bin")
	other := start("IC_DISPATCH_ID=" + id + "-other")

	e := sweepEngine("/proc")
	look := e.sweepEnvironment(id, false)
	if look.Found != 2 || look.Killed != 0 || !look.Alive() {
		t.Fatalf("look-only sweep = %+v, want 2 found, none killed", look)
	}
	if !alive(a) || !alive(b) {
		t.Fatal("a look-only sweep signalled a process")
	}
	got := e.sweepEnvironment(id, true)
	if got.Found != 2 || got.Killed != 2 {
		t.Fatalf("sweep = %+v, want 2 found and killed", got)
	}
	if got.Alive() {
		t.Fatal("every found process was killed, but the sweep reports one alive")
	}
	if alive(a) || alive(b) {
		t.Fatal("a candidate survived the sweep")
	}
	if !alive(clear) || !alive(other) {
		t.Fatal("the sweep killed a process that does not carry the id")
	}
}

// sleeperWithEnv starts a long sleep in its own session with exactly the given
// environment and returns its pid.
func sleeperWithEnv(t *testing.T, env ...string) int {
	t.Helper()
	cmd := exec.Command("sleep", "120")
	cmd.Env = append([]string{"PATH=/usr/bin:/bin"}, env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return cmd.Process.Pid
}
