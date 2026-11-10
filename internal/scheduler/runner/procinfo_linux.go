//go:build linux

package runner

import (
	"fmt"
	"os"
	"strings"
)

// ReadStat returns /proc/<pid>/stat.
func ReadStat(pid int) (string, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// SelfStartTime is this process's start time in clock ticks since boot.
func SelfStartTime() (uint64, error) {
	text, err := ReadStat(os.Getpid())
	if err != nil {
		return 0, err
	}
	_, st, err := ParseStat(text)
	return st, err
}

// BootID is the kernel's id for this boot.
func BootID() string {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
