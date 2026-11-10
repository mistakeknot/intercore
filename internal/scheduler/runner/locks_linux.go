//go:build icsched_test && linux

package runner

import (
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"syscall"
)

// ErrHeld means a lifetime lock is held by another process.
var ErrHeld = errors.New("lock held")

// QueueSocketName is the abstract unix socket name of the host-wide queue
// lock, without the leading "@" or NUL (plan section 6, lock 2).
func QueueSocketName(queueID string) string { return "intercore-scheduler/" + queueID }

var queueIDRe = regexp.MustCompile(`^[0-9a-f]{1,64}$`)

// LockDB takes flock(LOCK_EX|LOCK_NB) on the DB file's inode through a
// separate read-only descriptor (lock 1). Every alias of the inode (symlink,
// hardlink, bind mount) contends for the same lock. The caller keeps the file
// open for the life of the process and never closes it early: the kernel
// releases the lock at death, and nothing else may. os.OpenFile sets
// O_CLOEXEC, so a spawned agent cannot inherit the descriptor.
func LockDB(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s for the runner lock: %w", path, err)
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s", ErrHeld, path)
		}
		return nil, fmt.Errorf("flock %s: %w", path, err)
	}
	return f, nil
}

// LockQueue binds the abstract unix socket for queueID (lock 2). The kernel
// frees the name when the last descriptor closes, which is process death. A
// second bind, or a squatter, gets EADDRINUSE and is reported as ErrHeld. Go's
// net.Listen creates the socket close-on-exec.
func LockQueue(queueID string) (net.Listener, error) {
	if !queueIDRe.MatchString(queueID) {
		return nil, fmt.Errorf("invalid queue id %q", queueID)
	}
	ln, err := net.Listen("unix", "@"+QueueSocketName(queueID))
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			return nil, fmt.Errorf("%w: queue lock %s", ErrHeld, queueID)
		}
		return nil, fmt.Errorf("bind queue lock: %w", err)
	}
	return ln, nil
}
