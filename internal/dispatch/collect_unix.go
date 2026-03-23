//go:build !windows

package dispatch

import (
	"syscall"
	"time"
)

func isProcessAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil
}

func killProcess(pid int) {
	// Try SIGTERM first
	syscall.Kill(pid, syscall.SIGTERM)

	// Wait up to 5 seconds for graceful shutdown
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		if !isProcessAlive(pid) {
			return
		}
	}

	// Escalate to SIGKILL
	syscall.Kill(pid, syscall.SIGKILL)
}
