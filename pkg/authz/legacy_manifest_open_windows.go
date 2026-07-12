//go:build windows

package authz

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func openLegacyManifestNoFollow(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|windows.O_FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info); err != nil {
		_ = f.Close()
		return nil, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		_ = f.Close()
		return nil, fmt.Errorf("%q is a reparse point", path)
	}
	return f, nil
}
