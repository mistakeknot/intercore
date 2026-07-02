//go:build windows

package dispatch

import (
	"os"
	"time"
)

func isProcessAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// On Windows, os.FindProcess always succeeds. Probe with a nil signal
	// to check if the process is still running.
	err = p.Signal(os.Signal(nil))
	if err == nil {
		return true
	}
	return err.Error() == "os: process not finished"
}

func killProcess(pid int) {
	p, err := os.FindProcess(pid)
	if err != nil {
		return
	}

	// On Windows there is no SIGTERM equivalent via os.Process.
	// We first attempt a graceful kill, then escalate.
	// os.Process.Signal(os.Interrupt) sends CTRL_BREAK_EVENT on Windows.
	_ = p.Signal(os.Interrupt)

	// Wait up to 5 seconds for graceful shutdown
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		if !isProcessAlive(pid) {
			return
		}
	}

	// Escalate to hard kill (TerminateProcess)
	p.Kill()
}
