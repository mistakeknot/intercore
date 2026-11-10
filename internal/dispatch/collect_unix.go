//go:build !windows

package dispatch

import (
	"fmt"
	"syscall"
	"time"
)

// terminateRecordedProcess ends the recorded process group, escalating to SIGKILL
// after about five seconds.
func terminateRecordedProcess(identity processIdentity) error {
	return terminateRecordedProcessUntil(identity, nil)
}

// terminateRecordedProcessUntil is terminateRecordedProcess that gives up
// waiting when stop closes. SIGTERM has already been sent by then and no SIGKILL
// follows, so the group is left as the caller found it after the polite signal.
// A nil stop never fires.
func terminateRecordedProcessUntil(identity processIdentity, stop <-chan struct{}) error {
	if identity.PID <= 1 || identity.GroupID != identity.PID {
		return fmt.Errorf("process identity no longer matches admitted attempt")
	}
	alive, err := processIdentityAlive(identity)
	if err != nil || !alive {
		return err
	}
	// Retain birth identities of existing members before signalling. A surviving
	// member proves this is still the original group even after its leader exits.
	witnesses, err := processGroupMembers(identity.GroupID)
	if err != nil {
		return fmt.Errorf("process group inspection unavailable: %w", err)
	}
	witnesses = append(witnesses, identity)
	owned := func() bool {
		for _, witness := range witnesses {
			if matchesProcessIdentity(witness) {
				return true
			}
		}
		return false
	}
	if !owned() {
		return fmt.Errorf("process group ownership unavailable")
	}
	if err := syscall.Kill(-identity.GroupID, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
		return err
	}
	for i := 0; i < 50; i++ {
		if err := syscall.Kill(-identity.GroupID, 0); err == syscall.ESRCH {
			return nil
		}
		if !owned() {
			return fmt.Errorf("process group ownership lost during shutdown")
		}
		select {
		case <-stop:
			return ErrTerminationAbandoned
		case <-time.After(100 * time.Millisecond):
		}
	}
	// stop and the timer can both be ready on the last pass; stop wins.
	select {
	case <-stop:
		return ErrTerminationAbandoned
	default:
	}
	if !owned() {
		return fmt.Errorf("process group ownership lost before escalation")
	}
	if err := syscall.Kill(-identity.GroupID, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		return err
	}
	return nil
}

func isProcessAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	// EPERM means inspection/signalling is denied, not that the process exited.
	return err != syscall.ESRCH
}

func killProcess(pid int) {
	if pid <= 1 {
		return
	}
	// Only for a freshly started child which this caller has not reaped. Its PID
	// cannot be recycled. Detached row-based cancellation must use birth proof.
	syscall.Kill(-pid, syscall.SIGTERM)

	// Wait up to 5 seconds for graceful shutdown
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		if err := syscall.Kill(-pid, 0); err == syscall.ESRCH {
			return
		}
	}

	// Escalate to SIGKILL
	syscall.Kill(-pid, syscall.SIGKILL)
}
