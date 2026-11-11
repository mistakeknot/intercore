package runner

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseStat extracts the process state letter and the start time (clock ticks
// since boot, field 22) from the text of /proc/<pid>/stat. The command name is
// the one field that may contain spaces and parentheses, so the parse starts
// after the last ")".
func ParseStat(stat string) (state byte, startTime uint64, err error) {
	i := strings.LastIndexByte(stat, ')')
	if i < 0 || i+2 >= len(stat) {
		return 0, 0, fmt.Errorf("malformed stat line")
	}
	fields := strings.Fields(stat[i+1:])
	// fields[0] is field 3 (state); field 22 is fields[19].
	if len(fields) < 20 || len(fields[0]) != 1 {
		return 0, 0, fmt.Errorf("malformed stat line: %d fields after the command", len(fields))
	}
	st, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("stat start time: %w", err)
	}
	return fields[0][0], st, nil
}

// ParseStatPgrp is ParseStat plus the process group (field 5).
func ParseStatPgrp(stat string) (state byte, pgrp int, startTime uint64, err error) {
	state, startTime, err = ParseStat(stat)
	if err != nil {
		return 0, 0, 0, err
	}
	i := strings.LastIndexByte(stat, ')')
	fields := strings.Fields(stat[i+1:])
	// fields[2] is field 5 (pgrp); ParseStat checked there are at least 20.
	g, perr := strconv.Atoi(fields[2])
	if perr != nil {
		return 0, 0, 0, fmt.Errorf("stat process group: %w", perr)
	}
	return state, g, startTime, nil
}

// Holder is what a singleton row says about the runner that last held the queue.
type Holder struct {
	PID       int
	StartTime uint64
	BootID    string
}

// Present reports whether the recorded runner process still exists. A
// different boot id means the machine rebooted and it cannot. Otherwise the pid
// must exist with the same start time, so a reused pid is not mistaken for it.
// A stopped (T) or zombie (Z) process counts as present: it may resume, and a
// rebind must not proceed over it. read returns the stat text for a pid, or an
// error when the process does not exist.
func (h Holder) Present(bootID string, read func(pid int) (string, error)) bool {
	if h.PID <= 0 {
		return false
	}
	if h.BootID != "" && bootID != "" && h.BootID != bootID {
		return false
	}
	text, err := read(h.PID)
	if err != nil {
		return false
	}
	_, start, err := ParseStat(text)
	if err != nil {
		// Unreadable but existing: treat as present rather than risk a rebind.
		return true
	}
	return start == h.StartTime
}
