//go:build linux

package dispatch

import "syscall"

const cgroupFDSupported = true

// applyCgroupFD starts the child directly in the cgroup behind fd
// (CLONE_INTO_CGROUP), so there is no window in which it runs outside it.
func applyCgroupFD(a *syscall.SysProcAttr, fd int) {
	a.UseCgroupFD = true
	a.CgroupFD = fd
}
