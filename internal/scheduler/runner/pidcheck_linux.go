//go:build icsched_test && linux

package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/mistakeknot/intercore/internal/dispatch"
)

// The recorded-PID check (plan section 5.3d, row 7). The previous runner
// recorded the identity of the process it started (pid, process group, and a
// birth of boot id plus start time). A restarted runner adopts that process only
// when it is provably the same process; every other answer is a state the
// recovery table maps to an action.

type pidState int

const (
	pidAliveMatch  pidState = iota // same start time, boot id and group, not a zombie: adopt
	pidZombie                      // same start time, state Z or X: it has exited
	pidDead                        // no such process, or a different start time
	pidUnreadable                  // permission error, hidepid, or two stat reads that disagree
	pidProofAbsent                 // the identity row lacks a field: treated as unreadable
	pidPgidChanged                 // same start time, different process group: treated as unreadable
)

func (s pidState) String() string {
	switch s {
	case pidAliveMatch:
		return "alive_match"
	case pidZombie:
		return "zombie"
	case pidDead:
		return "dead"
	case pidUnreadable:
		return "unreadable"
	case pidProofAbsent:
		return "proof_absent"
	case pidPgidChanged:
		return "pgid_changed"
	}
	return "unknown"
}

// pidVerdict is the answer of one check. PidFD is an open pidfd only for
// pidAliveMatch with a real procfs, otherwise -1; the caller owns it.
type pidVerdict struct {
	State  pidState
	Detail string
	PidFD  int
}

// recordedIdentity is the JSON the dispatch package stores in table state under
// key dispatch.process. Its shape is read here and not through the dispatch
// package, which keeps it unexported.
type recordedIdentity struct {
	Version int    `json:"version"`
	PID     int    `json:"pid"`
	GroupID int    `json:"group_id"`
	Birth   string `json:"birth"`

	bootID    string
	startTime uint64
}

func (e *engine) procRoot() string {
	if h := e.o.hooks.procRoot; h != nil {
		if r := h(); r != "" {
			return r
		}
	}
	return "/proc"
}

// readRecordedIdentity loads and validates the identity row of a dispatch. A
// row that is missing, malformed or inconsistent with the dispatch's PID column
// is proof_absent: the check cannot tell whose process the PID is.
func readRecordedIdentity(ctx context.Context, sqlDB *sql.DB, d *dispatch.Dispatch) (*recordedIdentity, string) {
	if d == nil || d.PID == nil {
		return nil, "dispatch has no recorded PID"
	}
	var raw string
	err := sqlDB.QueryRowContext(ctx, `SELECT payload FROM state WHERE key='dispatch.process' AND scope_id=? AND expires_at IS NULL`, d.ID).Scan(&raw)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, "no recorded process identity"
	case err != nil:
		return nil, "read process identity: " + err.Error()
	}
	var id recordedIdentity
	if err := json.Unmarshal([]byte(raw), &id); err != nil {
		return nil, "process identity is not JSON"
	}
	if id.Version != 1 || id.PID <= 1 || id.PID != *d.PID || id.GroupID != id.PID || id.Birth == "" {
		return nil, "process identity is incomplete or does not match the dispatch"
	}
	parts := strings.SplitN(id.Birth, ":", 3)
	if len(parts) != 3 || parts[0] != "linux" || parts[1] == "" {
		return nil, "process identity birth is not a linux birth"
	}
	st, err := strconv.ParseUint(parts[2], 10, 64)
	if err != nil {
		return nil, "process identity start time is not a number"
	}
	id.bootID, id.startTime = parts[1], st
	return &id, ""
}

func goneErr(err error) bool {
	return errors.Is(err, unix.ESRCH) || errors.Is(err, unix.ENOENT)
}

// readStatAt reads <dirfd>/stat.
func readStatAt(dirfd int) (string, error) {
	fd, err := unix.Openat(dirfd, "stat", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", err
	}
	f := newFDReader(fd)
	defer f.close()
	b, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

type fdReader struct{ fd int }

func newFDReader(fd int) *fdReader { return &fdReader{fd: fd} }
func (r *fdReader) close()         { _ = unix.Close(r.fd) }
func (r *fdReader) Read(p []byte) (int, error) {
	n, err := unix.Read(r.fd, p)
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, io.EOF
	}
	return n, nil
}

// checkRecorded runs the check: open /proc/<pid> as a directory fd, read stat,
// pidfd_open, read stat again through the same directory fd. The directory fd
// refers to the process that was there at the first open, so a PID that is
// reused between the two reads fails the second read (ESRCH) instead of
// answering for a stranger, and the pidfd taken between the reads is the same
// process as both reads.
func (e *engine) checkRecorded(id *recordedIdentity) pidVerdict {
	v := pidVerdict{PidFD: -1}
	// Without this boot's id nothing ties a matching PID, group and start time to
	// the previous runner: a rebooted host can reuse all three. It is unproven,
	// so the five-minute rule applies and the process is never signalled.
	cur := e.o.bootID()
	if cur == "" {
		v.State, v.Detail = pidUnreadable, "current boot id is unavailable"
		return v
	}
	if id.bootID != cur {
		v.State, v.Detail = pidDead, "recorded boot id differs from this boot"
		return v
	}
	root := e.procRoot()
	dirfd, err := unix.Open(root+"/"+strconv.Itoa(id.PID), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		if goneErr(err) {
			v.State, v.Detail = pidDead, "no such process"
		} else {
			v.State, v.Detail = pidUnreadable, "open process directory: "+err.Error()
		}
		return v
	}
	defer unix.Close(dirfd)

	first, err := readStatAt(dirfd)
	if err != nil {
		return statFailure(v, err, "read stat")
	}
	state1, pgrp, start1, err := ParseStatPgrp(first)
	if err != nil {
		v.State, v.Detail = pidUnreadable, err.Error()
		return v
	}
	if start1 != id.startTime {
		v.State, v.Detail = pidDead, "start time differs: the PID belongs to another process"
		return v
	}

	// A fixture root cannot give a pidfd; only the real procfs does.
	pidfd := -1
	if root == "/proc" {
		pidfd, err = unix.PidfdOpen(id.PID, 0)
		if err != nil {
			if goneErr(err) {
				v.State, v.Detail = pidDead, "process exited before pidfd_open"
			} else {
				v.State, v.Detail = pidUnreadable, "pidfd_open: "+err.Error()
			}
			return v
		}
	}
	closeFD := func() {
		if pidfd >= 0 {
			_ = unix.Close(pidfd)
		}
	}

	second, err := readStatAt(dirfd)
	if err != nil {
		closeFD()
		return statFailure(v, err, "re-read stat")
	}
	state2, _, start2, err := ParseStatPgrp(second)
	if err != nil || start2 != start1 {
		closeFD()
		v.State, v.Detail = pidUnreadable, "the two stat reads disagree"
		return v
	}
	switch {
	case state1 == 'Z' || state1 == 'X' || state2 == 'Z' || state2 == 'X':
		closeFD()
		v.State, v.Detail = pidZombie, "process has exited and is not reaped"
	case pgrp != id.GroupID:
		closeFD()
		v.State, v.Detail = pidPgidChanged, fmt.Sprintf("process group is %d, recorded %d", pgrp, id.GroupID)
	default:
		v.State, v.Detail, v.PidFD = pidAliveMatch, "same start time, boot and process group", pidfd
	}
	return v
}

func statFailure(v pidVerdict, err error, what string) pidVerdict {
	if goneErr(err) {
		v.State, v.Detail = pidDead, what+": process is gone"
	} else {
		v.State, v.Detail = pidUnreadable, what+": "+err.Error()
	}
	return v
}
