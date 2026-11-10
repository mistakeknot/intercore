//go:build !linux

package dispatch

import "syscall"

const cgroupFDSupported = false

// applyCgroupFD is unreachable: Validate rejects CgroupFD off Linux.
func applyCgroupFD(*syscall.SysProcAttr, int) {}
