package runner

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
)

// ErrRefused marks a start the runner refuses for a safety reason. The CLI
// maps it to exit 1.
var ErrRefused = errors.New("scheduler runner refused")

func refuse(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrRefused, fmt.Sprintf(format, a...))
}

// CanonicalDB resolves a database path to an absolute canonical path.
func CanonicalDB(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", refuse("resolve %s: %v", path, err)
	}
	canon, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", refuse("resolve %s: %v", path, err)
	}
	return canon, nil
}

// CheckLiveDB refuses a database whose canonical path is under any
// /home/*/.clavain/ or under home's .clavain/ unless allowLive is set. The
// caller passes the canonical path, so a symlink into a live directory is
// refused too.
func CheckLiveDB(canonical, home string, allowLive bool) error {
	if allowLive {
		return nil
	}
	if home != "" {
		live := filepath.Join(filepath.Clean(home), ".clavain") + string(filepath.Separator)
		if strings.HasPrefix(canonical, live) {
			return refuse("%s is under %s; set IC_SCHEDULER_ALLOW_LIVE=1 to run a runner on a live database", canonical, live)
		}
	}
	if rest, ok := strings.CutPrefix(canonical, "/home/"); ok {
		user, tail, found := strings.Cut(rest, "/")
		if found && user != "" && (tail == ".clavain" || strings.HasPrefix(tail, ".clavain/")) {
			return refuse("%s is under /home/%s/.clavain/; set IC_SCHEDULER_ALLOW_LIVE=1 to run a runner on a live database", canonical, user)
		}
	}
	return nil
}

// MountPoints parses /proc/self/mountinfo and returns its mount points.
func MountPoints(r io.Reader) ([]string, error) {
	var out []string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 5 {
			continue
		}
		out = append(out, unescapeMountField(f[4]))
	}
	return out, sc.Err()
}

// unescapeMountField decodes the \ooo octal escapes mountinfo uses.
func unescapeMountField(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// IsMountPoint reports whether canonical is itself a mount point.
func IsMountPoint(canonical string, mountinfo io.Reader) (bool, error) {
	mps, err := MountPoints(mountinfo)
	if err != nil {
		return false, err
	}
	for _, m := range mps {
		if m == canonical {
			return true, nil
		}
	}
	return false, nil
}

// PathInfo is what the runner records and compares about the DB file.
type PathInfo struct {
	Canonical string
	Dev, Ino  uint64
	Nlink     uint64
	FSType    int64
}

// Identity is st_dev:st_ino.
func (p PathInfo) Identity() string { return fmt.Sprintf("%d:%d", p.Dev, p.Ino) }

// CheckPath applies the path and alias refusals that need no kernel access:
// hardlinks and a file bind mount.
func CheckPath(p PathInfo, mountinfo io.Reader) error {
	if p.Nlink > 1 {
		return refuse("%s has %d hard links; a hardlinked database can be opened under a second -wal", p.Canonical, p.Nlink)
	}
	mp, err := IsMountPoint(p.Canonical, mountinfo)
	if err != nil {
		return refuse("read mountinfo: %v", err)
	}
	if mp {
		return refuse("%s is a mount point (file bind mount)", p.Canonical)
	}
	return nil
}

// UnsafeFS names a filesystem type the runner will not lock on, or "".
func UnsafeFS(fsType int64) string {
	switch fsType {
	case 0x6969:
		return "nfs"
	case 0xFF534D42, 0xFE534D42:
		return "cifs"
	case 0x517B:
		return "smb"
	case 0x65735546:
		return "fuse"
	}
	return ""
}
