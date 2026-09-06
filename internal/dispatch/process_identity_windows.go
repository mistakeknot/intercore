//go:build windows

package dispatch

import (
	"fmt"

	"golang.org/x/sys/windows"
)

func windowsProcessBirth(handle windows.Handle) (string, error) {
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &created, &exited, &kernel, &user); err != nil {
		return "", err
	}
	return fmt.Sprintf("windows:%d:%d", created.HighDateTime, created.LowDateTime), nil
}

func readProcessIdentity(pid int) (processIdentity, error) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return processIdentity{}, err
	}
	defer windows.CloseHandle(handle)
	birth, err := windowsProcessBirth(handle)
	return processIdentity{Version: processIdentityVersion, PID: pid, GroupID: pid, Birth: birth}, err
}

func processGroupMembers(group int) ([]processIdentity, error) { return nil, nil }
