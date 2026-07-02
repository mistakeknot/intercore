//go:build !windows

package db

import (
	"fmt"
	"syscall"
)

func checkDiskSpace(dir string, minBytes uint64) error {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(dir, &stat); err != nil {
		return fmt.Errorf("cannot check disk space: %w", err)
	}
	available := stat.Bavail * uint64(stat.Bsize)
	if available < minBytes {
		return fmt.Errorf("disk full: %d bytes available (need %d)", available, minBytes)
	}
	return nil
}
