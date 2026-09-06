//go:build linux

package dispatch

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

func readProcessIdentity(pid int) (processIdentity, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return processIdentity{}, err
	}
	// comm is parenthesized and may contain spaces or parentheses.
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return processIdentity{}, fmt.Errorf("invalid process stat")
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 20 {
		return processIdentity{}, fmt.Errorf("short process stat")
	}
	group, err := strconv.Atoi(fields[2])
	if err != nil {
		return processIdentity{}, err
	}
	if _, err := strconv.ParseUint(fields[19], 10, 64); err != nil {
		return processIdentity{}, err
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return processIdentity{}, err
	}
	return processIdentity{Version: processIdentityVersion, PID: pid, GroupID: group, Birth: "linux:" + strings.TrimSpace(string(boot)) + ":" + fields[19]}, nil
}

func processGroupMembers(group int) ([]processIdentity, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var members []processIdentity
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		identity, err := readProcessIdentity(pid)
		if err == nil && identity.GroupID == group {
			members = append(members, identity)
		}
	}
	return members, nil
}
