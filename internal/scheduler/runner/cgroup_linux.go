//go:build icsched_test && linux

package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Containment (plan section 5.5). Every attempt runs in its own cgroup v2
// directory below a delegated subtree, started with CLONE_INTO_CGROUP so the
// child is never outside it. cgroup.kill then ends the whole attempt, and a
// "populated 0" in cgroup.events proves no process of it is left.
//
// Threat model: agents are buggy, not malicious (plan section 5.5(e) and
// section 13). The containment ends descendants that daemonize, setsid, clear
// their environment or fork in a storm. A process of the same uid can still
// write its own pid to another cgroup.procs inside the delegated subtree and
// leave the attempt cgroup; that is an escape by a malicious act, out of scope,
// and the runner's own unit cgroup still holds such a process.

const cgroup2Magic = 0x63677270 // CGROUP2_SUPER_MAGIC

// errSurvivorUnknown reports that populated 0 was not observed within the
// bound: some process of the attempt may still run.
var errSurvivorUnknown = errors.New("cgroup still populated after cgroup.kill")

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

type cgroupMgr struct {
	root  string // the delegated subtree
	leaf  string // the runner's own leaf directory inside it
	orig  string // the cgroup the runner started in, restored on release
	bound time.Duration
	delay func() time.Duration // test seam: holds back the populated 0 observation
	ns    string               // queue namespace of the attempt directories, set by setQueue
}

// setQueue namespaces the attempt directories by the queue's identity. Two
// queues that share a delegated subtree then never name the same attempt
// directory, so one queue's extinction cannot kill the other queue's worker.
// Within a queue the queue lock admits one runner, which makes the runner the
// only owner of its queue's attempt directories (an empty directory seen at
// create time is a leftover of this queue, not another runner's live attempt).
func (m *cgroupMgr) setQueue(queueID string) {
	sum := sha256.Sum256([]byte(queueID))
	m.ns = "q" + hex.EncodeToString(sum[:]) // the full digest: a short one can collide
}

// ownCgroupDir returns the filesystem path of the cgroup this process is in.
func ownCgroupDir() (string, error) {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "0::"); ok {
			return filepath.Join("/sys/fs/cgroup", filepath.Clean("/"+rest)), nil
		}
	}
	return "", errors.New("no cgroup v2 entry in /proc/self/cgroup")
}

// newCgroupMgr checks that the subtree is a writable cgroup v2 with cgroup.kill
// and moves the runner into a leaf of its own, so the subtree root holds no
// process. It returns an error the runner turns into a refusal.
func newCgroupMgr(o Options) (*cgroupMgr, error) {
	orig, err := ownCgroupDir()
	if err != nil {
		return nil, fmt.Errorf("cgroup v2 is not available: %w", err)
	}
	root := o.CgroupDir
	if root == "" {
		root = orig
	}
	var st unix.Statfs_t
	if err := unix.Statfs(root, &st); err != nil {
		return nil, fmt.Errorf("cgroup subtree %s: %w", root, err)
	}
	if st.Type != cgroup2Magic {
		return nil, fmt.Errorf("cgroup subtree %s is not a cgroup v2 directory", root)
	}
	m := &cgroupMgr{root: root, leaf: filepath.Join(root, "runner"), orig: orig, bound: o.ExtinctionBound, delay: o.hooks.populatedDelay}
	if err := os.Mkdir(m.leaf, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("cgroup subtree %s is not delegated to this user (cannot create %s): %w", root, m.leaf, err)
	}
	if _, err := os.Stat(filepath.Join(m.leaf, "cgroup.kill")); err != nil {
		return nil, fmt.Errorf("cgroup subtree %s has no cgroup.kill (kernel 5.14 or newer is required): %w", root, err)
	}
	if err := m.move(m.leaf); err != nil {
		return nil, fmt.Errorf("cannot move the runner into %s: %w", m.leaf, err)
	}
	return m, nil
}

func (m *cgroupMgr) move(dir string) error {
	return os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte(strconv.Itoa(os.Getpid())), 0o644)
}

// release puts the runner back where it started and removes its leaf. Best
// effort: a failure leaves an empty directory the next runner reuses.
func (m *cgroupMgr) release() {
	if m == nil {
		return
	}
	if m.orig != m.leaf {
		_ = m.move(m.orig)
	}
	_ = os.Remove(m.leaf)
}

// attemptDirName names the attempt's cgroup. A job ID made only of safe
// characters (and short) is used as is; any other ID is sanitized, truncated
// and tagged "~<hash of the full ID>". Safe IDs never contain "~", so two
// different job IDs never share a directory, which would let one job's
// extinction kill another job's live worker.
func attemptDirName(ns, jobID string, attemptNo int) string {
	id := unsafeName.ReplaceAllString(jobID, "_")
	if id != jobID || len(id) > 64 {
		sum := sha256.Sum256([]byte(jobID))
		if len(id) > 64 {
			id = id[:64]
		}
		id += "~" + hex.EncodeToString(sum[:8])
	}
	if ns != "" {
		// ns has a fixed width, so (ns, id) pairs never share a name.
		id = ns + "-" + id
	}
	return "attempt-" + id + "-" + strconv.Itoa(attemptNo)
}

var errCgroupBusy = errors.New("attempt cgroup already has, or may have, live processes")

// create makes the attempt's cgroup and returns its path and an open directory
// descriptor for CgroupFD. The caller closes the descriptor after Spawn. An
// existing directory is reused only when it is empty (a leftover of a path that
// never reached finalize, or of a restored database); one that still holds
// processes is refused with errCgroupBusy, and its path is still returned so
// the caller can record it and have the completion step kill and prove it empty
// before anything else runs for the job.
func (m *cgroupMgr) create(jobID string, attemptNo int) (string, *os.File, error) {
	p := filepath.Join(m.root, attemptDirName(m.ns, jobID, attemptNo))
	if err := os.Mkdir(p, 0o755); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return "", nil, err
		}
		busy, perr := populated(p)
		if perr != nil {
			// Not knowing whether it is empty is treated as busy: the path is
			// kept so the completion step must kill and prove it.
			return p, nil, fmt.Errorf("%s: %w (cgroup.events unreadable: %v)", p, errCgroupBusy, perr)
		}
		if busy {
			return p, nil, fmt.Errorf("%s: %w", p, errCgroupBusy)
		}
	}
	f, err := os.OpenFile(p, os.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = os.Remove(p)
		return "", nil, err
	}
	return p, f, nil
}

// populated reads cgroup.events. A missing directory counts as empty.
func populated(dir string) (bool, error) {
	b, err := os.ReadFile(filepath.Join(dir, "cgroup.events"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "populated "); ok {
			return strings.TrimSpace(v) != "0", nil
		}
	}
	return false, fmt.Errorf("no populated line in %s/cgroup.events", dir)
}

// extinct ends every process of the attempt and proves it: it writes 1 to
// cgroup.kill, then waits on cgroup.events (inotify, with a short poll as a
// backstop) for populated 0, for at most the bound. It returns nil only after
// that observation, errSurvivorUnknown on timeout. A directory that is already
// gone has nothing left in it.
func (m *cgroupMgr) extinct(dir string) error {
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	ifd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return fmt.Errorf("inotify: %w", err)
	}
	defer unix.Close(ifd)
	if _, err := unix.InotifyAddWatch(ifd, filepath.Join(dir, "cgroup.events"), unix.IN_MODIFY); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return fmt.Errorf("watch %s/cgroup.events: %w", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup.kill"), []byte("1"), 0o644); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("cgroup.kill %s: %w", dir, err)
	}
	start := time.Now()
	deadline := start.Add(m.bound)
	earliest := start
	if m.delay != nil {
		earliest = start.Add(m.delay())
	}
	buf := make([]byte, 4096)
	for {
		pop, err := populated(dir)
		if err != nil {
			return err
		}
		if !pop && !time.Now().Before(earliest) {
			return nil
		}
		now := time.Now()
		if !now.Before(deadline) {
			return fmt.Errorf("%w: %s after %s", errSurvivorUnknown, dir, m.bound)
		}
		wait := 100 * time.Millisecond
		if r := deadline.Sub(now); r < wait {
			wait = r
		}
		if !pop {
			if r := earliest.Sub(now); r < wait {
				wait = r
			}
		}
		fds := []unix.PollFd{{Fd: int32(ifd), Events: unix.POLLIN}}
		if _, err := unix.Poll(fds, int(wait.Milliseconds())+1); err != nil && !errors.Is(err, unix.EINTR) {
			return fmt.Errorf("poll: %w", err)
		}
		for { // drain the queued events
			if _, err := unix.Read(ifd, buf); err != nil {
				break
			}
		}
	}
}

// remove deletes the attempt's empty cgroup directory. It is called after the
// finalize commits; a directory that cannot be removed is only logged by the
// caller, since the job is already final.
func (m *cgroupMgr) remove(dir string) error {
	err := os.Remove(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// pathFor is the directory this runner would create for (job, attempt) in its
// queue. A recorded cgroup path of an older epoch is acted on only when it
// equals this exact recomputation: the path in the database is data, and a
// damaged or foreign value must never become a cgroup.kill target.
func (m *cgroupMgr) pathFor(jobID string, attemptNo int) string {
	return filepath.Join(m.root, attemptDirName(m.ns, jobID, attemptNo))
}

// procCount is the number of processes currently in the cgroup (lines of
// cgroup.procs). A missing directory holds none.
func procCount(dir string) (int, error) {
	b, err := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n := 0
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n, nil
}
