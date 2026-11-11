//go:build icsched_test && linux

package runner

import (
	"bytes"
	"io"
	"os"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

// The fallback-mode environment sweep (plan section 5.3d, row 8 without a
// cgroup). With no attempt cgroup, a dispatch that has no recorded PID, or
// whose wrapper is gone, may still have descendants. Every process of the same
// uid whose environment carries IC_DISPATCH_ID=<id> is a candidate. The sweep
// never adopts and never proves absence: a clean sweep and a failed sweep end
// the same way (the caller terminalizes indeterminate and blocks the retry).
// Its only job is to stop what it can prove belongs to the dispatch.

// sweepWait bounds how long a killed candidate may take to show as exited.
const sweepWait = 3 * time.Second

// sweepOutcome counts what one sweep saw.
type sweepOutcome struct {
	Found    int // candidates whose environment carried the dispatch id
	Killed   int // candidates SIGKILLed through a pidfd and seen to exit
	Unproven int // candidates or processes that could not be read, killed or seen to exit
}

// Clean reports that nothing was found and nothing was unreadable. Even then
// it proves nothing: the caller never treats it as proof of absence.
func (o sweepOutcome) Clean() bool { return o.Found == 0 && o.Unproven == 0 }

// Alive reports that the sweep positively identified a process of the dispatch
// that it did not kill. The legacy rule leaves such a process alone. Unproven
// processes are not counted: a process whose environment cannot be read is not
// known to belong to the dispatch, and on a real host some always exist.
func (o sweepOutcome) Alive() bool { return o.Found > o.Killed }

// sweepEnvironment scans the procfs root for processes of dispatchID. With
// kill false it only looks (the legacy rule); with kill true it SIGKILLs each
// candidate through a pidfd, which exists only for the real /proc.
func (e *engine) sweepEnvironment(dispatchID string, kill bool) sweepOutcome {
	var out sweepOutcome
	root := e.procRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		// A procfs that cannot be listed proves nothing about any process.
		e.logf("sweep %s: list %s: %v", dispatchID, root, err)
		out.Unproven++
		return out
	}
	self := os.Getpid()
	uid := uint32(os.Getuid())
	want := []byte("IC_DISPATCH_ID=" + dispatchID)
	for _, ent := range entries {
		pid, err := strconv.Atoi(ent.Name())
		if err != nil || pid <= 1 || pid == self {
			continue
		}
		e.sweepOne(root, pid, uid, want, kill, &out)
	}
	return out
}

func (e *engine) sweepOne(root string, pid int, uid uint32, want []byte, kill bool, out *sweepOutcome) {
	dirfd, err := unix.Open(root+"/"+strconv.Itoa(pid), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		if !goneErr(err) && !os.IsPermission(err) {
			out.Unproven++
		}
		return
	}
	defer unix.Close(dirfd)

	var st unix.Stat_t
	if err := unix.Fstat(dirfd, &st); err != nil || st.Uid != uid {
		return // another user's process is not this runner's to look at
	}
	first, err := readStatAt(dirfd)
	if err != nil {
		if !goneErr(err) {
			out.Unproven++
		}
		return
	}
	state1, _, start1, err := ParseStatPgrp(first)
	if err != nil {
		out.Unproven++
		return
	}
	if state1 == 'Z' || state1 == 'X' {
		return // it has exited; a zombie keeps no environment
	}
	env, err := readFileAt(dirfd, "environ")
	if err != nil {
		if !goneErr(err) {
			// A candidate that cannot be read counts as not absent.
			out.Unproven++
		}
		return
	}
	if !environHas(env, want) {
		// Not a candidate. Confirm it is still the same process, so a PID that
		// was reused mid-read is not mistaken for one that never matched.
		if _, bad := recheck(dirfd, start1); bad {
			out.Unproven++
		}
		return
	}
	out.Found++
	pidfd := -1
	if root == "/proc" {
		pidfd, err = unix.PidfdOpen(pid, 0)
		if err != nil {
			if goneErr(err) {
				out.Found--
				return
			}
			out.Unproven++
			return
		}
		defer unix.Close(pidfd)
	}
	if alive, bad := recheck(dirfd, start1); bad {
		out.Unproven++
		return
	} else if !alive {
		out.Found-- // it exited on its own between the reads
		return
	}
	if !kill || pidfd < 0 {
		return // counted as found and not killed: it stays "alive"
	}
	if err := unix.PidfdSendSignal(pidfd, unix.SIGKILL, nil, 0); err != nil {
		if goneErr(err) {
			out.Killed++
			return
		}
		out.Unproven++
		return
	}
	fds := []unix.PollFd{{Fd: int32(pidfd), Events: unix.POLLIN}}
	n, err := unix.Poll(fds, int(sweepWait/time.Millisecond))
	for err == unix.EINTR {
		n, err = unix.Poll(fds, int(sweepWait/time.Millisecond))
	}
	if err != nil || n == 0 {
		out.Unproven++
		return
	}
	out.Killed++
}

// recheck re-reads stat through the directory descriptor. A process that has
// exited since the first read is gone (alive false, not bad); a changed start
// time or any other read failure is unproven (bad).
func recheck(dirfd int, start uint64) (alive, bad bool) {
	text, err := readStatAt(dirfd)
	if err != nil {
		return false, !goneErr(err)
	}
	state, _, s, err := ParseStatPgrp(text)
	if err != nil || s != start {
		return false, true
	}
	return state != 'Z' && state != 'X', false
}

func readFileAt(dirfd int, name string) ([]byte, error) {
	fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := newFDReader(fd)
	defer f.close()
	return io.ReadAll(io.LimitReader(f, 8<<20))
}

// environHas reports whether the NUL-separated environment holds want as an
// entry of its own (not as the tail of another variable's value).
func environHas(env, want []byte) bool {
	for _, kv := range bytes.Split(env, []byte{0}) {
		if bytes.Equal(kv, want) {
			return true
		}
	}
	return false
}
