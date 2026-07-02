//go:build windows

package dispatch

import "syscall"

// platformSysProcAttr returns the SysProcAttr for Windows systems.
// CREATE_NEW_PROCESS_GROUP (0x00000200) gives the child its own
// console process group for clean signal handling.
func platformSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: 0x00000200}
}
