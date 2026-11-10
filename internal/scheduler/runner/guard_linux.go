//go:build linux

package runner

import (
	"fmt"
	"os"
	"syscall"
)

// StatDB stats the canonical DB path.
func StatDB(canonical string) (PathInfo, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(canonical, &st); err != nil {
		return PathInfo{}, refuse("stat %s: %v", canonical, err)
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(canonical, &fs); err != nil {
		return PathInfo{}, refuse("statfs %s: %v", canonical, err)
	}
	return PathInfo{Canonical: canonical, Dev: uint64(st.Dev), Ino: st.Ino, Nlink: uint64(st.Nlink), FSType: int64(fs.Type)}, nil
}

// NetNS returns the inode of this process's network namespace.
func NetNS() (uint64, error) {
	var st syscall.Stat_t
	if err := syscall.Stat("/proc/self/ns/net", &st); err != nil {
		return 0, fmt.Errorf("stat /proc/self/ns/net: %w", err)
	}
	return st.Ino, nil
}

// Preflight runs every refusal that needs no DB access, in the plan's order.
func Preflight(dbPath string, allowLive bool) (PathInfo, error) {
	canon, err := CanonicalDB(dbPath)
	if err != nil {
		return PathInfo{}, err
	}
	home, _ := os.UserHomeDir()
	if err := CheckLiveDB(canon, home, allowLive); err != nil {
		return PathInfo{}, err
	}
	info, err := StatDB(canon)
	if err != nil {
		return PathInfo{}, err
	}
	if fs := UnsafeFS(info.FSType); fs != "" {
		return PathInfo{}, refuse("%s is on a %s filesystem; flock is not reliable there", canon, fs)
	}
	mi, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return PathInfo{}, refuse("open mountinfo: %v", err)
	}
	defer mi.Close()
	if err := CheckPath(info, mi); err != nil {
		return PathInfo{}, err
	}
	return info, nil
}
