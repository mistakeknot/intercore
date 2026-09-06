//go:build !darwin && !linux && !windows

package dispatch

import "fmt"

func readProcessIdentity(pid int) (processIdentity, error) {
	return processIdentity{}, fmt.Errorf("process birth identity unsupported on this host")
}

func processGroupMembers(group int) ([]processIdentity, error) {
	return nil, fmt.Errorf("process group inspection unsupported on this host")
}
