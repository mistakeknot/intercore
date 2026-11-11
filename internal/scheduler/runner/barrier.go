//go:build icsched_test && linux

package runner

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Barriers and fault hooks (plan section 9). They exist only under the
// icsched_test build tag, so a shipped binary has none of them. A test drives
// them from the environment:
//
//	IC_SCHED_BARRIER=<point>[,<point>...]   the points to stop at
//	IC_SCHED_BARRIER_FIFO=<path>            a FIFO the test created
//
// At the first arrival at a listed point the runner appends "<point> <token>"
// to "<fifo>.hit", then blocks opening the FIFO for reading and reads until the
// writer closes it. A test therefore waits for the .hit line, acts (SIGSTOP or
// kill the runner, cancel the job, copy the database) and releases the runner
// by writing to the FIFO. Later arrivals at the same point pass.
//
//	IC_SCHED_FAULT=<fault>[,<fault>...]
//
//	busy:<kind>:<n>          the next n marker, evidence, collect or finalize steps fail
//	delay_populated:<secs>   hold back the populated 0 observation by that long
//	eagain_start:<n>         the next n Spawn calls fail at Start with EAGAIN
//	drop_entry:<point>       drop the in-flight map entry at the first arrival at the point
//	clock_shift:<secs>       the runner's clock runs that many seconds ahead
//	boot_id:<id>             the boot_id this runner records and compares (a "reboot" between two runs)
//	procfs_root_file:<path>  the recorded-PID check reads the procfs root named in this file while it exists
//	clock_offset_file:<path> the five minute rule's clock runs ahead by the seconds in this file
const (
	envBarrier = "IC_SCHED_BARRIER"
	envFIFO    = "IC_SCHED_BARRIER_FIFO"
	envFault   = "IC_SCHED_FAULT"
)

// barrierPoints is the closed set, in plan order.
var barrierPoints = []string{
	"after_claim",
	"in_link_before_commit",
	"after_link_before_start",
	"after_start_before_identity",
	"after_identity_before_running_cas",
	"after_exit_before_evidence",
	"after_evidence_before_collect",
	"after_collect_before_finalize",
	"in_finalize_before_commit",
	"after_finalize_commit",
	"after_launch_result_commit",
}

func validBarrier(p string) bool {
	for _, q := range barrierPoints {
		if p == q {
			return true
		}
	}
	return false
}

type fifoBarrier struct {
	points map[string]bool
	fifo   string
	mu     sync.Mutex
	seen   map[string]bool
}

func (b *fifoBarrier) at(point, token string) {
	if !b.points[point] {
		return
	}
	b.mu.Lock()
	first := !b.seen[point]
	b.seen[point] = true
	b.mu.Unlock()
	if !first {
		return
	}
	if f, err := os.OpenFile(b.fifo+".hit", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		fmt.Fprintf(f, "%s %s\n", point, token)
		f.Close()
	}
	r, err := os.OpenFile(b.fifo, os.O_RDONLY, 0)
	if err != nil {
		return
	}
	defer r.Close()
	_, _ = io.Copy(io.Discard, r)
}

// installEnvHooks fills the hooks a test did not set from the environment. A
// malformed value is reported on stderr and ignored: a test that depends on it
// fails on its own assertion.
func (o *Options) installEnvHooks() {
	if spec := os.Getenv(envBarrier); spec != "" && o.hooks.barrier == nil {
		fifo := os.Getenv(envFIFO)
		b := &fifoBarrier{points: map[string]bool{}, fifo: fifo, seen: map[string]bool{}}
		for _, p := range strings.Split(spec, ",") {
			p = strings.TrimSpace(p)
			switch {
			case fifo == "":
				fmt.Fprintf(o.Stderr, "ic scheduler run: %s needs %s\n", envBarrier, envFIFO)
			case !validBarrier(p):
				fmt.Fprintf(o.Stderr, "ic scheduler run: unknown barrier %q\n", p)
			default:
				b.points[p] = true
			}
		}
		if len(b.points) > 0 {
			o.hooks.barrier = b.at
		}
	}
	spec := os.Getenv(envFault)
	if spec == "" {
		return
	}
	var (
		mu    sync.Mutex
		busy  = map[string]int{}
		eagn  int
		delay time.Duration
		shift time.Duration
	)
	for _, f := range strings.Split(spec, ",") {
		f = strings.TrimSpace(f)
		parts := strings.Split(f, ":")
		num := func(i int) (int, bool) {
			if i >= len(parts) {
				return 0, false
			}
			n, err := strconv.Atoi(parts[i])
			return n, err == nil && n >= 0
		}
		switch parts[0] {
		case "busy":
			n, ok := num(2)
			if ok && len(parts) == 3 {
				busy[parts[1]] += n
				continue
			}
		case "delay_populated":
			if n, ok := num(1); ok {
				delay = time.Duration(n) * time.Second
				continue
			}
		case "eagain_start":
			if n, ok := num(1); ok {
				eagn += n
				continue
			}
		case "drop_entry":
			if len(parts) == 2 && validBarrier(parts[1]) {
				o.hooks.dropEntryAt = parts[1]
				continue
			}
		case "boot_id":
			if len(parts) == 2 && parts[1] != "" && o.hooks.bootID == nil {
				id := parts[1]
				o.hooks.bootID = func() string { return id }
				continue
			}
		case "procfs_root_file":
			// The procfs root the recorded-PID check reads is the content of
			// this file while it exists and is not empty; otherwise /proc.
			if len(parts) >= 2 && parts[1] != "" && o.hooks.procRoot == nil {
				path := strings.Join(parts[1:], ":")
				o.hooks.procRoot = func() string {
					b, err := os.ReadFile(path)
					if err != nil {
						return ""
					}
					return strings.TrimSpace(string(b))
				}
				continue
			}
		case "clock_offset_file":
			// The clock of the five minute rule runs ahead by the number of
			// seconds in this file while it exists.
			if len(parts) >= 2 && parts[1] != "" && o.hooks.clock == nil {
				path := strings.Join(parts[1:], ":")
				o.hooks.clock = func() time.Time {
					now := time.Now()
					if b, err := os.ReadFile(path); err == nil {
						if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
							now = now.Add(time.Duration(n) * time.Second)
						}
					}
					return now
				}
				continue
			}
		case "clock_shift":
			if n, ok := num(1); ok {
				shift = time.Duration(n) * time.Second
				continue
			}
		}
		fmt.Fprintf(o.Stderr, "ic scheduler run: unknown fault %q\n", f)
	}
	if len(busy) > 0 && o.hooks.inject == nil {
		o.hooks.inject = func(kind string) error {
			mu.Lock()
			defer mu.Unlock()
			if busy[kind] > 0 {
				busy[kind]--
				return fmt.Errorf("injected SQLITE_BUSY (%s)", kind)
			}
			return nil
		}
	}
	if delay > 0 && o.hooks.populatedDelay == nil {
		o.hooks.populatedDelay = func() time.Duration { return delay }
	}
	if eagn > 0 && o.hooks.startFault == nil {
		o.hooks.startFault = func() error {
			mu.Lock()
			defer mu.Unlock()
			if eagn > 0 {
				eagn--
				return fmt.Errorf("fork/exec: resource temporarily unavailable (injected EAGAIN)")
			}
			return nil
		}
	}
	if shift != 0 {
		base := o.Now
		if base == nil {
			base = time.Now
		}
		o.Now = func() time.Time { return base().Add(shift) }
	}
}
