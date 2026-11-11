//go:build icsched_test && linux

package runner

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mistakeknot/intercore/internal/scheduler"
)

// Slice 4d: the legacy rule (row 1). A legacy=1 attempt is never adopted and
// never re-run: a live or unprovably-gone process is left alone and the job is
// quarantined legacy_running; otherwise the evidence pipeline runs and the
// capped legacy finalize applies (ok completes, another definite class fails
// with it, indeterminate or unknown is quarantined legacy_unknown).
func TestLegacyRule(t *testing.T) {
	const pid, tk = 4100, 900
	birth := fmt.Sprintf("linux:boot-A:%d", tk)
	type tc struct {
		name     string
		dispatch string // dispatches.status
		pid      int
		birth    string // recorded identity, "" for none
		root     func(t *testing.T, did string) string
		prune    bool
		cancel   bool
		reboot   bool
		status   string
		outcome  string
	}
	live := func(state byte, pgrp int) func(t *testing.T, did string) string {
		return func(t *testing.T, did string) string { return fixtureProc(t, pid, state, pgrp, tk) }
	}
	empty := func(t *testing.T, did string) string { return t.TempDir() }
	cases := []tc{
		{name: "completed dispatch is completed", dispatch: "completed", root: empty, status: "completed", outcome: scheduler.ClassOK},
		{name: "failed dispatch without a verdict is legacy_unknown", dispatch: "failed", root: empty, status: "quarantined", outcome: scheduler.ReasonLegacyUnknown},
		{name: "pruned dispatch is legacy_unknown", dispatch: "spawned", prune: true, root: empty, status: "quarantined", outcome: scheduler.ReasonLegacyUnknown},
		{name: "completed dispatch of a cancelled job is cancelled", dispatch: "completed", cancel: true, root: empty, status: "cancelled", outcome: "cancelled"},
		{name: "live matching process is legacy_running", dispatch: "running", pid: pid, birth: birth, root: live('S', pid), status: "quarantined", outcome: scheduler.ReasonLegacyRunning},
		{name: "live process with no identity is legacy_running", dispatch: "running", pid: pid, root: live('S', pid), status: "quarantined", outcome: scheduler.ReasonLegacyRunning},
		{name: "live process in another group is legacy_running", dispatch: "running", pid: pid, birth: birth, root: live('S', pid+5), status: "quarantined", outcome: scheduler.ReasonLegacyRunning},
		{name: "exited zombie goes through the pipeline", dispatch: "running", pid: pid, birth: birth, root: live('Z', pid), status: "quarantined", outcome: scheduler.ReasonLegacyUnknown},
		{name: "dead process goes through the pipeline", dispatch: "running", pid: pid, birth: birth, root: empty, status: "quarantined", outcome: scheduler.ReasonLegacyUnknown},
		{name: "no identity after a reboot goes through the pipeline", dispatch: "running", pid: pid, root: live('S', pid), reboot: true, status: "quarantined", outcome: scheduler.ReasonLegacyUnknown},
		{name: "no PID and a swept process is legacy_running", dispatch: "spawned", status: "quarantined", outcome: scheduler.ReasonLegacyRunning,
			root: func(t *testing.T, did string) string {
				root := t.TempDir()
				envProc(t, root, 5001, 'S', "IC_DISPATCH_ID="+did)
				return root
			}},
		{name: "no PID and a clean sweep goes through the pipeline", dispatch: "spawned", root: empty, status: "quarantined", outcome: scheduler.ReasonLegacyUnknown},
		{name: "no PID after a reboot goes through the pipeline", dispatch: "spawned", reboot: true, status: "quarantined", outcome: scheduler.ReasonLegacyUnknown,
			root: func(t *testing.T, did string) string {
				root := t.TempDir()
				envProc(t, root, 5001, 'S', "IC_DISPATCH_ID="+did) // never looked at
				return root
			}},
		{name: "live process of a cancelling job stays cancelling", dispatch: "running", pid: pid, birth: birth, cancel: true, root: live('S', pid), status: "cancelling"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			o := r.opts()
			ep := r.firstEpoch(o, "boot-A")
			cl := r.claim("x", ep)
			r.launchAs(cl, scheduler.LaunchStarted)
			did := r.linked(cl, c.dispatch, c.pid)
			// The PIDs here are synthetic: forget them before the rig's reap
			// signals the group of every running dispatch it still finds.
			t.Cleanup(func() { _, _ = r.raw.Exec(`UPDATE dispatches SET pid = NULL`) })
			if c.birth != "" {
				r.identity(did, c.pid, c.birth, c.pid)
			}
			r.paths(cl, "none", "")
			if _, err := r.raw.Exec(`UPDATE scheduler_job_attempts SET legacy = 1 WHERE claim_token = ?`, cl.Token); err != nil {
				t.Fatal(err)
			}
			if c.dispatch == "running" || c.dispatch == "spawned" {
				if err := r.store.MarkRunning(context.Background(), cl, time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			if c.prune {
				conn, err := r.raw.Conn(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				for _, q := range []string{`PRAGMA foreign_keys = OFF`, `DELETE FROM dispatches WHERE id = '` + did + `'`, `PRAGMA foreign_keys = ON`} {
					if _, err := conn.ExecContext(context.Background(), q); err != nil {
						t.Fatal(err)
					}
				}
				conn.Close()
			}
			if c.cancel {
				r.cancel("x")
			}
			root := c.root(t, did)
			o.hooks.procRoot = func() string { return root }
			boot := "boot-A"
			if c.reboot {
				boot = "boot-B"
			}
			var got snap
			lg := r.recovered(o, boot, func() { got = r.snap("x") })
			if got.Status != c.status || (c.outcome != "" && got.Outcome != c.outcome) {
				t.Fatalf("job %+v, want %s %s\n%s", got, c.status, c.outcome, lg)
			}
			if r.runs() != 0 {
				t.Fatalf("the executor ran %d times", r.runs())
			}
			if n := r.count(`SELECT COUNT(*) FROM scheduler_job_attempts WHERE job_id = 'x' AND legacy = 0 AND claim_token != ?`, cl.Token); n != 0 {
				t.Fatalf("%d non-legacy attempts exist: a legacy attempt was adopted or re-run", n)
			}
			if c.status == "quarantined" && c.outcome == scheduler.ReasonLegacyRunning && c.dispatch != "completed" {
				if st, _, _ := r.dispatchRow(did); st != c.dispatch {
					t.Fatalf("dispatch %q, want %q untouched", st, c.dispatch)
				}
			}
		})
	}
}

// A live legacy process is never signalled: with the real procfs, a real
// process whose identity matches is still running after the pass, and so is
// one found only by the environment sweep.
func TestLegacyLiveProcessIsNeverSignalled(t *testing.T) {
	for _, withPID := range []bool{true, false} {
		name := "recorded process"
		if !withPID {
			name = "swept process"
		}
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			o := r.opts()
			ep := r.firstEpoch(o, "boot-A")
			cl := r.claim("x", ep)
			r.launchAs(cl, scheduler.LaunchStarted)
			var did string
			var pid int
			if withPID {
				pid = sleeper(t)
				did = r.linked(cl, "running", pid)
				b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
				if err != nil {
					t.Fatal(err)
				}
				_, _, start, err := ParseStatPgrp(string(b))
				if err != nil {
					t.Fatal(err)
				}
				r.identity(did, pid, fmt.Sprintf("linux:boot-A:%d", start), pid)
			} else {
				did = r.linked(cl, "spawned", 0)
				pid = sleeperWithEnv(t, "IC_DISPATCH_ID="+did)
			}
			r.paths(cl, "none", "")
			if _, err := r.raw.Exec(`UPDATE scheduler_job_attempts SET legacy = 1 WHERE claim_token = ?`, cl.Token); err != nil {
				t.Fatal(err)
			}
			if err := r.store.MarkRunning(context.Background(), cl, time.Now()); err != nil {
				t.Fatal(err)
			}
			var got snap
			lg := r.recovered(o, "boot-A", func() { got = r.snap("x") })
			if got.Status != "quarantined" || got.Outcome != scheduler.ReasonLegacyRunning {
				t.Fatalf("job %+v, want quarantined legacy_running\n%s", got, lg)
			}
			if !alive(pid) {
				t.Fatal("the legacy process was signalled")
			}
			if !strings.Contains(lg, "left alone") {
				t.Fatalf("no 'left alone' line in the log:\n%s", lg)
			}
		})
	}
}
