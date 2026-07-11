//go:build !windows

package runtimeproof

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func openRegularNoFollow(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	if f == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("%q could not be opened", path)
	}
	return f, nil
}
