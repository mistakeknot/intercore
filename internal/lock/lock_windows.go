//go:build windows

package lock

import (
	"os"
)

// pidAlive checks if a process with the given PID is running.
// On Windows, os.FindProcess always succeeds, so we attempt to
// read the process handle to determine liveness.
func pidAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// On Windows, Signal(os.Signal(syscall.Signal(0))) returns an error
	// if the process does not exist. We use a zero signal to probe.
	err = p.Signal(os.Signal(nil))
	// If err is nil the process is alive (unlikely on Windows without
	// elevated privileges). If "not finished", the process is still running.
	// Any "finished" or access-denied error means dead or inaccessible.
	if err == nil {
		return true
	}
	return err.Error() == "os: process not finished"
}
