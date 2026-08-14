package publish

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Process liveness for the cache prune.
//
// WHY THIS EXISTS
//
// A plugin cache version directory can be the working set of a process that is
// running right now. Deleting it does not stop that process — Unix keeps an
// executing inode alive after its last link is gone — so nothing appears to
// break. What breaks is everything downstream of being able to NAME what is
// running: the manifest beside the binary is gone, so no tool can report the
// process's version, and the artifact can never be inspected again.
//
// That is the BEST case, and it only holds for a self-contained binary. A
// process that reads more of its directory as it runs loses the rest of it:
// measured alongside the intermux servers, seven python processes were running
// from an intersearch .venv whose directory had been removed, where every
// not-yet-executed import is a file that is no longer there. Anything
// interpreted, plugin hooks read on demand, and lazily-loaded assets fail at
// use rather than at delete — which is to say, later, and somewhere else.
//
// Measured on Clavain 2026-08-11: `ic publish` pruned intermux 0.1.9 while 23
// intermux-mcp servers were executing it. They ran on for days, unnameable. The
// existing protections could not have caught it — installed_plugins.json and
// the marketplace guard both describe what SHOULD be running, and the
// .orphaned_at grace window is a 24-hour timer, while the oldest of those
// processes had been up 17 days. Only asking the process table can answer
// "is anyone still using this".

// ProcExe is a running process and the executable path it is running.
type ProcExe struct {
	PID int
	Exe string
}

// psTimeout bounds the process-table read. A publish that hangs here would
// stall after the artifact is already live, which is the worst place to stop.
const psTimeout = 10 * time.Second

// RunningExecutables enumerates running processes and resolves each one's
// executable path.
//
// Branches on evidence rather than on runtime.GOOS: /proc is used when it
// actually yields processes, otherwise ps. Sniffing the platform instead would
// make the untaken branch untestable on the machine you are standing on.
func RunningExecutables() []ProcExe {
	if procs, ok := runningFromProc("/proc"); ok {
		return procs
	}
	return runningFromPS()
}

// runningFromProc reads executable paths from a procfs. The bool reports
// whether this procfs yielded any process at all, which is what distinguishes
// "not Linux" from "Linux with nothing to report".
func runningFromProc(procRoot string) ([]ProcExe, bool) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, false
	}
	var out []ProcExe
	sawPID := false
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // non-numeric procfs entries are not processes
		}
		sawPID = true
		exe, err := os.Readlink(filepath.Join(procRoot, e.Name(), "exe"))
		if err != nil {
			continue // short-lived process, or one we may not inspect
		}
		// Linux appends this marker once the binary is unlinked — which is
		// precisely the state this file exists to prevent, so it must still
		// resolve to the directory it came from.
		out = append(out, ProcExe{PID: pid, Exe: strings.TrimSuffix(exe, " (deleted)")})
	}
	return out, sawPID
}

// runningFromPS reads executable paths from ps. On macOS the comm column is the
// full executable path; on systems where it is only a name, the entry cannot be
// resolved to a directory and is skipped by heldVersionDirs.
func runningFromPS() []ProcExe {
	ctx, cancel := context.WithTimeout(context.Background(), psTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,comm=").Output()
	if err != nil {
		return nil
	}
	return parsePS(string(out))
}

// parsePS is the pure core of runningFromPS. comm may contain spaces, so the
// line is split exactly once.
func parsePS(out string) []ProcExe {
	var procs []ProcExe
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		if len(parts) != 2 {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(parts[0]))
		if err != nil {
			continue
		}
		procs = append(procs, ProcExe{PID: pid, Exe: strings.TrimSpace(parts[1])})
	}
	return procs
}

// heldVersionDirs maps each cache version directory to the processes executing
// out of it. Pure: the caller supplies the cache root and the process list.
//
// A cache version directory is <root>/<marketplace>/<plugin>/<version>, so an
// executable belongs to one when its path has at least one component below that
// depth. Anything shallower is not a plugin artifact.
func heldVersionDirs(root string, procs []ProcExe) map[string][]ProcExe {
	held := make(map[string][]ProcExe)
	if root == "" {
		return held
	}
	root = filepath.Clean(root)

	// Match against the resolved root as well as the given one. ps and procfs
	// report REAL paths, so on macOS a cache reached through a symlinked
	// component (/var -> /private/var, and any symlink in HOME) arrives here
	// spelled differently than the caller spells it. Matching only one form
	// silently attributes nothing, which turns this interlock into a no-op —
	// the failure mode it exists to prevent, arrived at by a different road.
	//
	// Keys stay in the CALLER'S namespace, because that is how the prune spells
	// the paths it is about to delete.
	roots := []string{root}
	if resolved, err := filepath.EvalSymlinks(root); err == nil && resolved != root {
		roots = append(roots, resolved)
	}

	for _, p := range procs {
		if p.Exe == "" || !filepath.IsAbs(p.Exe) {
			continue // a bare process name cannot be attributed to a directory
		}
		exe := filepath.Clean(p.Exe)
		for _, r := range roots {
			rel, err := filepath.Rel(r, exe)
			if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
				continue
			}
			parts := strings.Split(rel, string(filepath.Separator))
			if len(parts) < 4 {
				continue // need marketplace/plugin/version/<file>
			}
			dir := filepath.Join(root, parts[0], parts[1], parts[2])
			held[dir] = append(held[dir], p)
			break
		}
	}
	return held
}
