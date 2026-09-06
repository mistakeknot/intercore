//go:build darwin

package dispatch

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func readProcessIdentity(pid int) (processIdentity, error) {
	p, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return processIdentity{}, err
	}
	boot, err := unix.Sysctl("kern.bootsessionuuid")
	if err != nil {
		return processIdentity{}, err
	}
	if boot == "" {
		return processIdentity{}, fmt.Errorf("empty boot-session UUID")
	}
	return darwinProcessIdentity(p, boot), nil
}

func darwinProcessIdentity(p *unix.KinfoProc, boot string) processIdentity {
	return processIdentity{Version: processIdentityVersion, PID: int(p.Proc.P_pid), GroupID: int(p.Eproc.Pgid), Birth: fmt.Sprintf("darwin:%s:%d:%d", boot, p.Proc.P_starttime.Sec, p.Proc.P_starttime.Usec)}
}

func processGroupMembers(group int) ([]processIdentity, error) {
	all, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	boot, err := unix.Sysctl("kern.bootsessionuuid")
	if err != nil {
		return nil, err
	}
	if boot == "" {
		return nil, fmt.Errorf("empty boot-session UUID")
	}
	var members []processIdentity
	for _, p := range all {
		if int(p.Eproc.Pgid) == group {
			members = append(members, darwinProcessIdentity(&p, boot))
		}
	}
	return members, nil
}
