//go:build !windows

package dispatch

import "syscall"

// platformSysProcAttr returns the SysProcAttr for Unix systems.
// Setpgid creates a new process group for clean signal propagation.
func platformSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
