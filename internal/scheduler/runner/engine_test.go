//go:build icsched_test && linux

package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mistakeknot/intercore/internal/scheduler"
)

// setPaused writes the global pause flag the way `ic scheduler pause` does.
func setPaused(t *testing.T, raw *sql.DB, paused bool) {
	t.Helper()
	if err := writePause(raw, paused); err != nil {
		t.Fatal(err)
	}
}

// writePause is setPaused for goroutines that must not call t.Fatal.
func writePause(raw *sql.DB, paused bool) error {
	v := "false"
	if paused {
		v = "true"
	}
	_, err := raw.Exec(`INSERT INTO state (key, scope_id, payload, updated_at) VALUES ('scheduler_paused', 'global', ?, 1)
		ON CONFLICT(key, scope_id) DO UPDATE SET payload = excluded.payload`, v)
	return err
}

// rig is one throwaway queue with the fake executor and a log of its runs.
type rig struct {
	t     *testing.T
	path  string
	store *scheduler.Store
	raw   *sql.DB
	dir   string // project dir, prompts and outputs
	log   string // $FAKE_LOG
}

func fakeDispatchSH(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs("testdata/fake-dispatch.sh")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func newRig(t *testing.T) *rig {
	t.Helper()
	path, store, raw := newQueue(t)
	r := &rig{t: t, path: path, store: store, raw: raw, dir: t.TempDir()}
	r.log = filepath.Join(r.dir, "fake.log")
	t.Setenv("FAKE_LOG", r.log)
	t.Cleanup(r.reap)
	return r
}

// reap kills any executor a test left running, by the process group recorded
// for its dispatch, so a failed test does not leak sleeping children.
func (r *rig) reap() {
	rows, err := r.raw.Query(`SELECT pid FROM dispatches WHERE pid IS NOT NULL AND pid > 1 AND status IN ('running','spawned')`)
	if err != nil {
		return
	}
	defer rows.Close()
	var pids []int
	for rows.Next() {
		var p int
		if rows.Scan(&p) == nil {
			pids = append(pids, p)
		}
	}
	for _, p := range pids {
		_ = syscall.Kill(-p, syscall.SIGKILL)
		_ = syscall.Kill(p, syscall.SIGKILL)
	}
}

func (r *rig) opts() Options {
	return Options{
		DB:             r.path,
		DispatchSH:     fakeDispatchSH(r.t),
		Tick:           5 * time.Millisecond,
		HeartbeatEvery: 20 * time.Millisecond,
		RetryBase:      5 * time.Millisecond,
		RetryMax:       20 * time.Millisecond,
		RepairEvery:    50 * time.Millisecond,
		DrainTimeout:   10 * time.Second,
		MaxConcurrent:  2,
	}
}

// jobOpt adjusts a job before it is created.
type jobOpt func(*scheduler.SpawnJob)

// job creates a pending job whose prompt's first line is mode. It returns the
// prompt path, which a "gate" mode job needs for its release file.
func (r *rig) job(id, mode string, mut ...jobOpt) string {
	r.t.Helper()
	prompt := filepath.Join(r.dir, id+".prompt")
	if err := os.WriteFile(prompt, []byte(mode+"\n"), 0o600); err != nil {
		r.t.Fatal(err)
	}
	so, _ := json.Marshal(map[string]any{"AgentType": "codex", "PromptFile": prompt})
	j := &scheduler.SpawnJob{
		ID: id, Status: scheduler.StatusPending, Priority: scheduler.PriorityNormal, AgentType: "codex",
		SpawnOpts: string(so), MaxRetries: 0, CreatedAt: time.Unix(1_800_000_000, 0),
		ProjectDir: r.dir, OutputBase: filepath.Join(r.dir, "out-"+id),
	}
	for _, m := range mut {
		m(j)
	}
	if err := r.store.Create(context.Background(), j); err != nil {
		r.t.Fatalf("create %s: %v", id, err)
	}
	return prompt
}

func (r *rig) get(id string) *scheduler.SpawnJob {
	r.t.Helper()
	j, err := r.store.Get(context.Background(), id)
	if err != nil {
		r.t.Fatal(err)
	}
	return j
}

func (r *rig) count(q string, args ...any) int {
	r.t.Helper()
	var n int
	if err := r.raw.QueryRow(q, args...).Scan(&n); err != nil {
		r.t.Fatalf("%s: %v", q, err)
	}
	return n
}

func (r *rig) attempts(id string) int {
	return r.count(`SELECT COUNT(*) FROM scheduler_job_attempts WHERE job_id = ?`, id)
}

// dispatches counts the dispatch rows linked to a job's attempts.
func (r *rig) dispatches(id string) int {
	return r.count(`SELECT COUNT(*) FROM scheduler_job_attempts WHERE job_id = ? AND dispatch_id IS NOT NULL`, id)
}

// runs counts executor invocations recorded in $FAKE_LOG.
func (r *rig) runs() int {
	b, err := os.ReadFile(r.log)
	if err != nil {
		return 0
	}
	return strings.Count(string(b), "run pid=")
}

func (r *rig) outcome(id string) string {
	var c sql.NullString
	if err := r.raw.QueryRow(`SELECT outcome_class FROM scheduler_jobs WHERE id = ?`, id).Scan(&c); err != nil {
		r.t.Fatal(err)
	}
	return c.String
}

// until polls cond for waitFor and fails with msg if it never holds. Polling
// is only used to observe a state the test already forced with a seam or a
// gate; it never stands in for ordering.
func until(t *testing.T, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitFor)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s", msg)
		}
		time.Sleep(time.Millisecond)
	}
}

func (r *rig) waitStatus(id string, want scheduler.JobStatus) {
	r.t.Helper()
	until(r.t, fmt.Sprintf("job %s to be %s (is %s)", id, want, r.get(id).Status), func() bool {
		return r.get(id).Status == want
	})
}

// release lets a "gate" job finish.
func release(t *testing.T, prompt string) {
	t.Helper()
	b, err := os.ReadFile(prompt)
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(b))
	if len(fields) < 2 {
		t.Fatalf("prompt %s has no gate file", prompt)
	}
	if err := os.WriteFile(fields[1], nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// stop cancels the runner and waits for a clean drain.
func (r *running) stop(t *testing.T) {
	t.Helper()
	r.cancel()
	select {
	case <-r.done:
	case <-time.After(waitFor):
		t.Fatal("runner did not drain")
	}
	if r.err != nil {
		t.Fatalf("drain: %v", r.err)
	}
}

// TestHappyPathCompletesOnce (I4): one job runs, completes, and leaves one
// attempt, one dispatch and one executor run.
func TestHappyPathCompletesOnce(t *testing.T) {
	r := newRig(t)
	r.job("j1", "ok")
	run := start(t, r.opts())
	r.waitStatus("j1", scheduler.StatusCompleted)
	run.stop(t)

	if got := r.outcome("j1"); got != scheduler.ClassOK {
		t.Fatalf("outcome %q", got)
	}
	if n := r.attempts("j1"); n != 1 {
		t.Fatalf("attempts = %d", n)
	}
	if n := r.dispatches("j1"); n != 1 {
		t.Fatalf("dispatches = %d", n)
	}
	if n := r.runs(); n != 1 {
		t.Fatalf("executor runs = %d", n)
	}
	if n := r.count(`SELECT COUNT(*) FROM dispatches`); n != 1 {
		t.Fatalf("dispatch rows = %d", n)
	}
}
