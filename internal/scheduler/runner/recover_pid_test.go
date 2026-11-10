//go:build icsched_test && linux

package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/mistakeknot/intercore/internal/dispatch"
	"github.com/mistakeknot/intercore/internal/scheduler"
)

// Slice 4c: the recorded-PID check, adoption and the five minute rule.

// statText builds a /proc/<pid>/stat line: field 3 is the state, 5 the process
// group, 22 the start time.
func statText(pid int, state byte, pgrp int, start uint64) string {
	f := make([]string, 0, 52)
	f = append(f, string(state), "1", strconv.Itoa(pgrp), "0", "0", "0")
	for len(f) < 19 {
		f = append(f, "0")
	}
	f = append(f, strconv.FormatUint(start, 10))
	for len(f) < 50 {
		f = append(f, "0")
	}
	return fmt.Sprintf("%d (a b) %s\n", pid, strings.Join(f, " "))
}

// fixtureProc writes <root>/<pid>/stat and returns the root.
func fixtureProc(t *testing.T, pid int, state byte, pgrp int, start uint64) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, strconv.Itoa(pid)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, strconv.Itoa(pid), "stat"), []byte(statText(pid, state, pgrp, start)), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func testEngine(root, boot string) *engine {
	e := &engine{}
	e.o.hooks.procRoot = func() string { return root }
	e.o.hooks.bootID = func() string { return boot }
	return e
}

func TestParseStatPgrp(t *testing.T) {
	st, pg, start, err := ParseStatPgrp(statText(7, 'S', 7, 4242))
	if err != nil || st != 'S' || pg != 7 || start != 4242 {
		t.Fatalf("got %c %d %d %v", st, pg, start, err)
	}
	if _, _, _, err := ParseStatPgrp("7 (x) S"); err == nil {
		t.Fatal("a short stat line parsed")
	}
}

// Unit 17, the check itself: each of the six states is produced from a procfs
// root fixture.
func TestRecordedPIDStates(t *testing.T) {
	const pid, tk, boot = 4000, 777, "boot-A"
	id := &recordedIdentity{Version: 1, PID: pid, GroupID: pid, Birth: fmt.Sprintf("linux:%s:%d", boot, tk), bootID: boot, startTime: tk}
	cases := []struct {
		name string
		root func(t *testing.T) string
		boot string
		want pidState
	}{
		{"alive_match", func(t *testing.T) string { return fixtureProc(t, pid, 'S', pid, tk) }, boot, pidAliveMatch},
		{"zombie", func(t *testing.T) string { return fixtureProc(t, pid, 'Z', pid, tk) }, boot, pidZombie},
		{"dead no such pid", func(t *testing.T) string { return t.TempDir() }, boot, pidDead},
		{"dead other start time", func(t *testing.T) string { return fixtureProc(t, pid, 'S', pid, tk+1) }, boot, pidDead},
		{"dead other boot", func(t *testing.T) string { return fixtureProc(t, pid, 'S', pid, tk) }, "boot-B", pidDead},
		{"pgid_changed", func(t *testing.T) string { return fixtureProc(t, pid, 'S', pid+9, tk) }, boot, pidPgidChanged},
		{"unreadable stat", func(t *testing.T) string {
			root := fixtureProc(t, pid, 'S', pid, tk)
			p := filepath.Join(root, strconv.Itoa(pid), "stat")
			_ = os.Remove(p)
			if err := os.Mkdir(p, 0o755); err != nil { // reading a directory fails
				t.Fatal(err)
			}
			return root
		}, boot, pidUnreadable},
		{"unreadable garbage", func(t *testing.T) string {
			root := fixtureProc(t, pid, 'S', pid, tk)
			_ = os.WriteFile(filepath.Join(root, strconv.Itoa(pid), "stat"), []byte("garbage"), 0o644)
			return root
		}, boot, pidUnreadable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := testEngine(tc.root(t), tc.boot).checkRecorded(id)
			if v.State != tc.want {
				t.Fatalf("state %s (%s), want %s", v.State, v.Detail, tc.want)
			}
			if v.PidFD >= 0 {
				t.Fatal("a fixture root produced a pidfd")
			}
		})
	}
}

// The real procfs yields a pidfd for a live process and a changed start time is
// dead; a PID that was reused cannot be adopted.
func TestRecordedPIDRealProcfs(t *testing.T) {
	pid := sleeper(t)
	text, err := ReadStat(pid)
	if err != nil {
		t.Fatal(err)
	}
	_, pgrp, tk, err := ParseStatPgrp(text)
	if err != nil {
		t.Fatal(err)
	}
	e := &engine{}
	e.o.hooks.bootID = func() string { return "b" }
	id := &recordedIdentity{Version: 1, PID: pid, GroupID: pgrp, bootID: "b", startTime: tk}
	v := e.checkRecorded(id)
	if v.State != pidAliveMatch || v.PidFD < 0 {
		t.Fatalf("live sleeper: %s (%s) fd=%d", v.State, v.Detail, v.PidFD)
	}
	_ = syscall.Close(v.PidFD)
	id.startTime = tk + 1 // the PID now belongs to a different process
	if v := e.checkRecorded(id); v.State != pidDead || v.PidFD >= 0 {
		t.Fatalf("reused PID: %s fd=%d", v.State, v.PidFD)
	}
}

func (r *rig) identity(did string, pid int, birth string, group int) {
	r.t.Helper()
	b, _ := json.Marshal(map[string]any{"version": 1, "pid": pid, "group_id": group, "birth": birth})
	if _, err := r.raw.Exec(`INSERT INTO state(key,scope_id,payload,updated_at) VALUES ('dispatch.process',?,?,unixepoch())`, did, string(b)); err != nil {
		r.t.Fatal(err)
	}
}

func TestReadRecordedIdentityProofAbsent(t *testing.T) {
	r := newRig(t)
	o := r.opts()
	ep := r.firstEpoch(o, "boot-A")
	c := r.claim("x", ep)
	r.launchAs(c, scheduler.LaunchStarted)
	did := r.linked(c, "running", 4001)
	d, err := dispatch.New(r.raw, nil).Get(context.Background(), did)
	if err != nil {
		t.Fatal(err)
	}
	if id, why := readRecordedIdentity(context.Background(), r.raw, d); id != nil || why == "" {
		t.Fatalf("no identity row: got %+v %q", id, why)
	}
	for name, mut := range map[string]string{
		"wrong version":  `{"version":2,"pid":4001,"group_id":4001,"birth":"linux:boot-A:5"}`,
		"other pid":      `{"version":1,"pid":4002,"group_id":4002,"birth":"linux:boot-A:5"}`,
		"group != pid":   `{"version":1,"pid":4001,"group_id":1,"birth":"linux:boot-A:5"}`,
		"no birth":       `{"version":1,"pid":4001,"group_id":4001}`,
		"darwin birth":   `{"version":1,"pid":4001,"group_id":4001,"birth":"darwin:1:2"}`,
		"bad start time": `{"version":1,"pid":4001,"group_id":4001,"birth":"linux:boot-A:x"}`,
		"not json":       `nope`,
	} {
		_, _ = r.raw.Exec(`DELETE FROM state WHERE key='dispatch.process'`)
		if _, err := r.raw.Exec(`INSERT INTO state(key,scope_id,payload,updated_at) VALUES ('dispatch.process',?,?,unixepoch())`, did, mut); err != nil {
			t.Fatal(err)
		}
		if id, why := readRecordedIdentity(context.Background(), r.raw, d); id != nil || why == "" {
			t.Fatalf("%s: accepted %+v", name, id)
		}
	}
	_, _ = r.raw.Exec(`DELETE FROM state WHERE key='dispatch.process'`)
	r.identity(did, 4001, "linux:boot-A:5", 4001)
	if id, why := readRecordedIdentity(context.Background(), r.raw, d); id == nil || id.startTime != 5 || id.bootID != "boot-A" {
		t.Fatalf("good row rejected: %+v %q", id, why)
	}
}

// pidRow is a started attempt with a linked running dispatch whose recorded
// process is pid, optionally inside a real attempt cgroup.
type pidRow struct {
	c   *scheduler.Claim
	did string
	cg  string
}

func (r *rig) pidRow(ep int64, pid int, birth string, group int, containment, cgroup string) pidRow {
	r.t.Helper()
	c := r.claim("x", ep)
	r.launchAs(c, scheduler.LaunchStarted)
	did := r.linked(c, "running", pid)
	if birth != "" {
		r.identity(did, pid, birth, group)
	}
	r.paths(c, containment, cgroup)
	if err := r.store.MarkRunning(context.Background(), c, time.Now()); err != nil {
		r.t.Fatal(err)
	}
	return pidRow{c: c, did: did, cg: cgroup}
}

// Unit 17, the mapping to actions: every state in fallback mode.
func TestRecoveryRecordedPIDActionsFallback(t *testing.T) {
	const pid, tk = 4100, 900
	birth := fmt.Sprintf("linux:boot-A:%d", tk)
	type tc struct {
		name   string
		root   func(t *testing.T) string
		birth  string
		action string // adopted | finalized | waiting | quarantined
	}
	cases := []tc{
		{"alive_match adopts", func(t *testing.T) string { return fixtureProc(t, pid, 'S', pid, tk) }, birth, "adopted"},
		{"zombie is exited", func(t *testing.T) string { return fixtureProc(t, pid, 'Z', pid, tk) }, birth, "finalized"},
		{"dead is exited", func(t *testing.T) string { return t.TempDir() }, birth, "finalized"},
		{"unreadable waits", func(t *testing.T) string {
			root := fixtureProc(t, pid, 'S', pid, tk)
			p := filepath.Join(root, strconv.Itoa(pid), "stat")
			_ = os.Remove(p)
			_ = os.Mkdir(p, 0o755)
			return root
		}, birth, "quarantined"},
		{"proof_absent waits", func(t *testing.T) string { return fixtureProc(t, pid, 'S', pid, tk) }, "", "quarantined"},
		{"pgid_changed waits", func(t *testing.T) string { return fixtureProc(t, pid, 'S', pid+5, tk) }, birth, "quarantined"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			o := r.opts()
			ep := r.firstEpoch(o, "boot-A")
			row := r.pidRow(ep, pid, c.birth, pid, "none", "")
			root := c.root(t)
			o.hooks.procRoot = func() string { return root }
			var shift atomic.Int64
			base := time.Now()
			o.hooks.clock = func() time.Time { return base.Add(time.Duration(shift.Load())) }
			o.RecoverEvery = 20 * time.Millisecond
			o.DrainTimeout = 300 * time.Millisecond // an adopted worker keeps running
			var passes atomic.Int32
			o.hooks.recoveryPass = func() { passes.Add(1) }
			o.hooks.bootID = func() string { return "boot-A" }
			lg := &logBuf{}
			o.Stderr = lg
			run := start(t, o)
			defer run.stop(t)
			until(t, "two recovery passes", func() bool { return passes.Load() >= 2 })
			got := r.snap("x")
			switch c.action {
			case "adopted":
				if got.Status != "running" || got.Token == row.c.Token || got.Token == "" {
					t.Fatalf("not adopted: %+v", got)
				}
				var lease int64
				if err := r.raw.QueryRow(`SELECT lease_epoch FROM scheduler_jobs WHERE id='x'`).Scan(&lease); err != nil || lease != run.epoch {
					t.Fatalf("lease_epoch %d err %v, want %d", lease, err, run.epoch)
				}
			case "finalized":
				if got.Outcome != scheduler.ClassIndeterminate || got.Token != "" {
					t.Fatalf("not finalized indeterminate: %+v", got)
				}
			case "quarantined":
				if got.Status != "running" || got.Token != row.c.Token {
					t.Fatalf("left flight before five minutes: %+v", got)
				}
				shift.Store(int64(5*time.Minute - time.Second))
				seen := passes.Load()
				until(t, "two more recovery passes", func() bool { return passes.Load() >= seen+2 })
				if q := r.snap("x"); q.Status != "running" || q.Token != row.c.Token {
					t.Fatalf("left flight at 4m59s: %+v", q)
				}
				shift.Store(int64(5*time.Minute + time.Second))
				until(t, "quarantine after five minutes", func() bool { return r.snap("x").Status == "quarantined" })
				if q := r.snap("x"); q.Outcome != scheduler.ReasonSurvivorUnknown {
					t.Fatalf("quarantine reason %q", q.Outcome)
				}
			}
		})
	}
}

// Unit 17 and P5, cgroup mode: an unreadable process is re-checked for five
// minutes, then the attempt cgroup is killed and proven empty and the job
// finishes indeterminate.
func TestRecoveryUnreadableCgroupModeKillsAfterFiveMinutes(t *testing.T) {
	r, o := containRig(t)
	row, err := r.store.ReadRunner(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ep := r.firstEpoch(o, "boot-A")
	m := &cgroupMgr{root: o.CgroupDir}
	m.setQueue(row.QueueID)
	c := r.claim("x", ep)
	p, f, err := m.create("x", c.AttemptNo)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sleep", "120")
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(f.Fd())}
	if err := cmd.Start(); err != nil {
		f.Close()
		t.Skipf("cannot start a process into %s: %v", p, err)
	}
	f.Close()
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	const fakePID = 4200
	r.launchAs(c, scheduler.LaunchStarted)
	did := r.linked(c, "running", fakePID)
	r.identity(did, fakePID, "linux:boot-A:900", fakePID)
	r.paths(c, "cgroup", p)
	if err := r.store.MarkRunning(context.Background(), c, time.Now()); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir() // the recorded PID is unreadable: not there at all would be dead, so make stat unreadable
	_ = os.MkdirAll(filepath.Join(root, strconv.Itoa(fakePID), "stat"), 0o755)
	o.hooks.procRoot = func() string { return root }
	var shift atomic.Int64
	base := time.Now()
	o.hooks.clock = func() time.Time { return base.Add(time.Duration(shift.Load())) }
	o.RecoverEvery = 20 * time.Millisecond
	var passes atomic.Int32
	o.hooks.recoveryPass = func() { passes.Add(1) }
	o.hooks.bootID = func() string { return "boot-A" }
	run := start(t, o)
	defer run.stop(t)
	until(t, "two recovery passes", func() bool { return passes.Load() >= 2 })
	if got := r.snap("x"); got.Status != "running" || got.Token != c.Token {
		t.Fatalf("the row left flight before five minutes: %+v", got)
	}
	select {
	case <-exited:
		t.Fatal("the cgroup was killed before five minutes")
	default:
	}
	// One second short of the bound, still nothing.
	shift.Store(int64(5*time.Minute - time.Second))
	seen := passes.Load()
	until(t, "two more recovery passes", func() bool { return passes.Load() >= seen+2 })
	if got := r.snap("x"); got.Status != "running" || got.Token != c.Token {
		t.Fatalf("the row left flight at 4m59s: %+v", got)
	}
	select {
	case <-exited:
		t.Fatal("the cgroup was killed at 4m59s")
	default:
	}
	shift.Store(int64(5*time.Minute + time.Second))
	until(t, "finalized", func() bool { return r.snap("x").Outcome == scheduler.ClassIndeterminate })
	select {
	case <-exited:
	case <-time.After(waitFor):
		t.Fatal("the attempt cgroup was not killed")
	}
	if pop, _ := populated(p); pop {
		t.Fatal("the cgroup was not proven empty")
	}
	if st, _, _ := r.dispatchRow(did); st != "failed" {
		t.Fatalf("dispatch status %q", st)
	}
}

// Adoption: the new token and the new epoch replace the old ones in the job
// row and the attempt row, the old token no longer resolves, and the adopted
// entry counts toward max_concurrent.
func TestAdoptSwapsTokenAndEpoch(t *testing.T) {
	r := newRig(t)
	o := r.opts()
	ep := r.firstEpoch(o, "boot-A")
	pid := sleeper(t)
	text, _ := ReadStat(pid)
	_, pg, tk, _ := ParseStatPgrp(text)
	row := r.pidRow(ep, pid, fmt.Sprintf("linux:boot-A:%d", tk), pg, "none", "")
	_ = pg
	o.hooks.bootID = func() string { return "boot-A" }
	o.DrainTimeout = 300 * time.Millisecond // the adopted worker keeps running
	lg := &logBuf{}
	o.Stderr = lg
	var passes atomic.Int32
	o.hooks.recoveryPass = func() { passes.Add(1) }
	run := start(t, o)
	until(t, "a recovery pass", func() bool { return passes.Load() >= 1 })
	got := r.snap("x")
	run.stop(t)
	if got.Status != "running" || got.Token == "" || got.Token == row.c.Token {
		t.Fatalf("not adopted: %+v\n%s", got, lg.String())
	}
	if !strings.Contains(lg.String(), "adopted") || !alive(pid) {
		t.Fatalf("adoption not logged or the process was signalled:\n%s", lg.String())
	}
	var n int
	if err := r.raw.QueryRow(`SELECT COUNT(*) FROM scheduler_job_attempts WHERE claim_token = ? AND lease_epoch = ?`, got.Token, run.epoch).Scan(&n); err != nil || n != 1 {
		t.Fatalf("attempt row not re-tokened: %d %v", n, err)
	}
}

func TestAdoptStoreGuards(t *testing.T) {
	r := newRig(t)
	o := r.opts()
	ep := r.firstEpoch(o, "boot-A")
	c := r.claim("x", ep)
	r.launchAs(c, scheduler.LaunchStarted)
	_ = r.linked(c, "running", 4300)
	ctx := context.Background()
	// The epoch of the call must be the singleton's epoch.
	if _, err := r.store.Adopt(ctx, "x", c.Token, "new-1", ep+1); err == nil {
		t.Fatal("adopted under a foreign epoch")
	}
	// A row of the current epoch is not in recovery scope.
	if _, err := r.store.Adopt(ctx, "x", c.Token, "new-1", ep); err == nil {
		t.Fatal("adopted a row of the current epoch")
	}
	if _, err := r.store.Adopt(ctx, "x", c.Token, c.Token, ep); err == nil {
		t.Fatal("adopted with equal tokens")
	}
	if _, err := r.store.Adopt(ctx, "x", "", "new-1", ep); err == nil {
		t.Fatal("adopted with an empty token")
	}
	if _, err := r.store.Adopt(ctx, "nope", c.Token, "new-1", ep); err == nil {
		t.Fatal("adopted a missing job")
	}
	if got := r.snap("x"); got.Token != c.Token {
		t.Fatalf("a refused adoption changed the row: %+v", got)
	}
}
