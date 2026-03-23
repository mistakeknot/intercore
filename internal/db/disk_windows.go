//go:build windows

package db

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

func checkDiskSpace(dir string, minBytes uint64) error {
	dirPtr, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return fmt.Errorf("cannot check disk space: %w", err)
	}
	var freeBytesAvailable uint64
	var totalBytes uint64
	var totalFreeBytes uint64
	err = windows.GetDiskFreeSpaceEx(
		dirPtr,
		(*uint64)(unsafe.Pointer(&freeBytesAvailable)),
		(*uint64)(unsafe.Pointer(&totalBytes)),
		(*uint64)(unsafe.Pointer(&totalFreeBytes)),
	)
	if err != nil {
		return fmt.Errorf("cannot check disk space: %w", err)
	}
	if freeBytesAvailable < minBytes {
		return fmt.Errorf("disk full: %d bytes available (need %d)", freeBytesAvailable, minBytes)
	}
	return nil
}
