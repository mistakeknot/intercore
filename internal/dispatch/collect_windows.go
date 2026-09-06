//go:build windows

package dispatch

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func terminateRecordedProcess(identity processIdentity) error {
	if identity.PID <= 1 || identity.Birth == "" || identity.Version != processIdentityVersion {
		return fmt.Errorf("invalid process identity")
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE, false, uint32(identity.PID))
	if err != nil {
		if err == windows.ERROR_INVALID_PARAMETER {
			return nil
		}
		return err
	}
	defer windows.CloseHandle(handle)
	birth, err := windowsProcessBirth(handle)
	if err != nil {
		return err
	}
	if birth != identity.Birth {
		return nil // the original process exited; never signal its PID's new owner
	}
	// The validated handle refers to this process even if its numeric PID is reused.
	return windows.TerminateProcess(handle, 1)
}

func isProcessAlive(pid int) bool {
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return err != windows.ERROR_INVALID_PARAMETER
	}
	defer windows.CloseHandle(handle)
	state, err := windows.WaitForSingleObject(handle, 0)
	if err != nil {
		return true
	}
	return state != windows.WAIT_OBJECT_0
}

func killProcess(pid int) {
	p, err := os.FindProcess(pid)
	if err != nil {
		return
	}

	// Fresh unreaped child only. Go does not implement Signal(os.Interrupt) on
	// Windows; do not describe an unsupported no-op as graceful cancellation.
	p.Kill()
}
