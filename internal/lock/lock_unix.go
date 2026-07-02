//go:build !windows

package lock

import (
	"errors"
	"syscall"
)

// pidAlive checks if a process with the given PID is running.
func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
