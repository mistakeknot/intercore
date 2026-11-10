//go:build icsched_test && linux

package runner

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func tempDB(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "q.db")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLockDBContendsAcrossAliases(t *testing.T) {
	db := tempDB(t)
	dir := filepath.Dir(db)
	sym := filepath.Join(t.TempDir(), "alias.db")
	if err := os.Symlink(db, sym); err != nil {
		t.Fatal(err)
	}
	hard := filepath.Join(dir, "hard.db")
	if err := os.Link(db, hard); err != nil {
		t.Fatal(err)
	}
	first, err := LockDB(db)
	if err != nil {
		t.Fatal(err)
	}
	for _, alias := range []string{db, sym, hard} {
		if f, err := LockDB(alias); !errors.Is(err, ErrHeld) {
			if f != nil {
				f.Close()
			}
			t.Fatalf("second lock through %s: %v", alias, err)
		}
	}
	// Negative control: once the holder closes, the lock is free.
	first.Close()
	again, err := LockDB(sym)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	again.Close()
}

func TestLockQueueExclusive(t *testing.T) {
	const q = "0123456789abcdef0123456789abcdef"
	first, err := LockQueue(q)
	if err != nil {
		t.Fatal(err)
	}
	if l, err := LockQueue(q); !errors.Is(err, ErrHeld) {
		if l != nil {
			l.Close()
		}
		t.Fatalf("second bind: %v", err)
	}
	other, err := LockQueue("fedcba9876543210fedcba9876543210")
	if err != nil {
		t.Fatalf("a different queue id must not contend: %v", err)
	}
	other.Close()
	first.Close()
	again, err := LockQueue(q)
	if err != nil {
		t.Fatalf("bind after release: %v", err)
	}
	again.Close()
}

// A squatter (any process that binds the name first) is reported as held.
func TestLockQueueSquatter(t *testing.T) {
	const q = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	sq, err := net.Listen("unix", "@"+QueueSocketName(q))
	if err != nil {
		t.Fatal(err)
	}
	defer sq.Close()
	if l, err := LockQueue(q); !errors.Is(err, ErrHeld) {
		if l != nil {
			l.Close()
		}
		t.Fatalf("got %v", err)
	}
}

func TestLockQueueRejectsBadID(t *testing.T) {
	for _, q := range []string{"", "../x", "A B", strings.Repeat("a", 65)} {
		if l, err := LockQueue(q); err == nil {
			l.Close()
			t.Errorf("queue id %q accepted", q)
		}
	}
}

func fdLinks(t *testing.T, pid int) []string {
	t.Helper()
	dir := "/proc/" + itoa(pid) + "/fd"
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		l, err := os.Readlink(filepath.Join(dir, e.Name()))
		if err == nil {
			out = append(out, l)
		}
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

// Unit test 11: a spawned child inherits neither lock descriptor, since an
// inherited descriptor would keep a lock alive after the runner died.
func TestLockDescriptorsAreCloseOnExec(t *testing.T) {
	db := tempDB(t)
	socksBefore := socketLinks(fdLinks(t, os.Getpid()))
	lockFile, err := LockDB(db)
	if err != nil {
		t.Fatal(err)
	}
	defer lockFile.Close()
	ln, err := LockQueue("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var ourSock []string
	for _, s := range socketLinks(fdLinks(t, os.Getpid())) {
		if !contains(socksBefore, s) {
			ourSock = append(ourSock, s)
		}
	}
	if len(ourSock) == 0 {
		t.Fatal("could not identify the queue lock socket in /proc/self/fd")
	}

	// Negative control: a descriptor opened without O_CLOEXEC IS inherited, so
	// the assertions below can fail.
	leakFd, err := syscall.Open(db, syscall.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(leakFd)

	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { child.Process.Kill(); child.Wait() }()
	links := fdLinks(t, child.Process.Pid)

	leaks := 0
	for _, l := range links {
		if l == db {
			leaks++
		}
		for _, s := range ourSock {
			if l == s {
				t.Errorf("child inherited the queue lock socket %s", s)
			}
		}
	}
	if leaks != 1 {
		t.Fatalf("child has %d descriptors on the DB file; want exactly the 1 deliberate leak (control), links %v", leaks, links)
	}
}

func socketLinks(links []string) []string {
	var out []string
	for _, l := range links {
		if strings.HasPrefix(l, "socket:[") {
			out = append(out, l)
		}
	}
	return out
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
