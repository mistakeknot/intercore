//go:build icsched_test && linux

package main

// Slice 4d, G2: a database that held 040-shape scheduler jobs is migrated by
// the current code and recovered by a fallback-mode runner. A legacy job is
// never adopted and never run again: a live worker is left alone and the job is
// quarantined legacy_running, a row with no dispatch link is quarantined
// legacy_unknown, and nothing is launched.

import (
	"bufio"
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mistakeknot/intercore/internal/db"
)

const legacy040DDL = `
CREATE TABLE scheduler_jobs (
    id          TEXT PRIMARY KEY,
    status      TEXT NOT NULL DEFAULT 'pending',
    priority    INTEGER NOT NULL DEFAULT 2,
    agent_type  TEXT NOT NULL DEFAULT 'codex',
    session_name TEXT,
    batch_id    TEXT,
    dispatch_id TEXT,
    spawn_opts  TEXT NOT NULL,
    max_retries INTEGER NOT NULL DEFAULT 3,
    retry_count INTEGER NOT NULL DEFAULT 0,
    error_msg   TEXT,
    created_at  INTEGER NOT NULL,
    started_at  INTEGER,
    completed_at INTEGER,
    FOREIGN KEY (dispatch_id) REFERENCES dispatches(id)
);
CREATE INDEX idx_scheduler_jobs_status ON scheduler_jobs(status);
CREATE INDEX idx_scheduler_jobs_session ON scheduler_jobs(session_name);
`

const legacyGoodOpts = `{"ProjectDir":"/work/proj","OutputFile":"/work/out/a","DispatchSH":"/opt/dispatch.sh"}`

// downgrade040 turns the migrated, empty database at path into one in the 040
// shape, as the 041 migration's own tests do.
func downgrade040(t *testing.T, path string) *sql.DB {
	t.Helper()
	d, err := db.Open(path, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	for _, stmt := range []string{
		"DROP TABLE IF EXISTS scheduler_job_attempts",
		"DROP TABLE IF EXISTS scheduler_runner",
		"DROP TABLE IF EXISTS scheduler_jobs",
		"DELETE FROM state WHERE key = 'scheduler_paused'",
		legacy040DDL,
		"PRAGMA user_version = 40",
	} {
		if _, err := d.SqlDB().Exec(stmt); err != nil {
			t.Fatalf("downgrade %q: %v", stmt, err)
		}
	}
	return d.SqlDB()
}

// legacySeed is one 040 job: its status, its dispatch (status "" = no
// dispatches row) and the pid that dispatch recorded.
type legacySeed struct {
	id, status, dispatch string
	pid                  int
	link                 bool // dispatch_id is set (to a row that may be pruned)
}

func seed040(t *testing.T, sdb *sql.DB, rows []legacySeed) {
	t.Helper()
	exec := func(q string, args ...any) {
		if _, err := sdb.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec("PRAGMA foreign_keys = OFF")
	defer exec("PRAGMA foreign_keys = ON")
	for i, r := range rows {
		created := int64(1700000000 + i)
		var link, pid any
		if r.link {
			link = "d-" + r.id
		}
		if r.dispatch != "" {
			if r.pid != 0 {
				pid = r.pid
			}
			exec(`INSERT INTO dispatches (id, project_dir, status, created_at, pid) VALUES (?, '/work/proj', ?, ?, ?)`,
				"d-"+r.id, r.dispatch, created+50, pid)
		}
		var started, completed any
		if r.status == "running" {
			started = created + 60
		}
		if r.status == "completed" || r.status == "failed" || r.status == "cancelled" {
			completed = created + 900
		}
		exec(`INSERT INTO scheduler_jobs (id, status, agent_type, spawn_opts, max_retries, retry_count, dispatch_id, created_at, started_at, completed_at)
			VALUES (?, ?, 'codex', ?, 3, 0, ?, ?, ?, ?)`, r.id, r.status, legacyGoodOpts, link, created, started, completed)
	}
}

func TestCLILegacyJobsAreNeverRunOrAdopted(t *testing.T) {
	r := newRecNoCgroup(t)
	live := exec.Command("sleep", "120")
	live.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := live.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = live.Wait() }()
	t.Cleanup(func() { _ = live.Process.Kill() })

	sdb := downgrade040(t, r.path)
	seed040(t, sdb, []legacySeed{
		{id: "run-live", status: "running", dispatch: "running", pid: live.Process.Pid, link: true},
		{id: "run-done", status: "running", dispatch: "completed", link: true},
		{id: "run-failed", status: "running", dispatch: "failed", link: true},
		{id: "run-gone", status: "running", dispatch: "", link: true},
		{id: "run-null", status: "running"},
		{id: "pend-null", status: "pending"},
		{id: "retry-null", status: "retrying"},
		{id: "done", status: "completed", dispatch: "completed", link: true},
		{id: "bad", status: "failed"},
	})
	d, err := db.Open(r.path, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	_ = d.Close()

	if n := r.e.count(r.path, `SELECT COUNT(*) FROM scheduler_job_attempts WHERE legacy = 1`); n != 4 {
		t.Fatalf("%d legacy attempts after migration, want 4 (the linked running rows)", n)
	}

	p := r.run(nil)
	p.waitReady(t)
	if _, se, code := r.e.ic("--db=q.db", "scheduler", "resume"); code != 0 {
		t.Fatalf("resume exit %d: %s", code, se)
	}
	deadline := time.Now().Add(waitLimit)
	for r.e.count(r.path, `SELECT COUNT(*) FROM scheduler_jobs WHERE status IN ('running','pending','scheduled','retrying')`) > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("jobs still unsettled; runner log:\n%s", p.stderr())
		}
		time.Sleep(20 * time.Millisecond)
	}
	r.stop(p)

	want := map[string][2]string{
		"run-live":   {"quarantined", "legacy_running"},
		"run-gone":   {"quarantined", "legacy_unknown"},
		"run-null":   {"quarantined", "legacy_unknown"},
		"pend-null":  {"quarantined", "legacy_unknown"},
		"retry-null": {"quarantined", "legacy_unknown"},
		"run-done":   {"completed", "ok"},
		"run-failed": {"quarantined", "legacy_unknown"},
		"done":       {"completed", ""},
		"bad":        {"failed", ""},
	}
	for id, w := range want {
		st, oc := r.e.jobOutcome(r.path, id)
		if st != w[0] || (w[1] != "" && oc != w[1]) {
			t.Errorf("job %s = %s %s, want %s %s", id, st, oc, w[0], w[1])
		}
	}
	if n := r.execs(); n != 0 {
		t.Errorf("the executor was launched %d times for legacy jobs", n)
	}
	if n := r.e.count(r.path, `SELECT COUNT(*) FROM scheduler_job_attempts WHERE legacy = 0`); n != 0 {
		t.Errorf("%d non-legacy attempts: a legacy job was adopted or run again", n)
	}
	if procGone(live.Process.Pid) {
		t.Error("the live legacy worker was signalled")
	}
}

// legacyCommit is the last commit of the 040 scheduler, before the runner.
const legacyCommit = "1b445cf"

// buildLegacyHost builds cmd/ic/testdata/legacyhost_main.go.txt inside an
// untouched export of the 040 tree and returns the binary. Nothing is added to
// this repository's module or its migrations: the export and the build live in
// a temporary directory. The test skips only when the commit is not available.
func buildLegacyHost(t *testing.T) string {
	t.Helper()
	repo, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("git", "-C", repo, "cat-file", "-e", legacyCommit+"^{commit}").Run(); err != nil {
		t.Skipf("the 040 source commit %s is not in this checkout", legacyCommit)
	}
	src := t.TempDir()
	archive := exec.Command("git", "-C", repo, "archive", legacyCommit)
	untar := exec.Command("tar", "-x", "-C", src)
	pipe, err := archive.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	untar.Stdin = pipe
	if err := untar.Start(); err != nil {
		t.Fatal(err)
	}
	if err := archive.Run(); err != nil {
		t.Fatalf("git archive: %v", err)
	}
	if err := untar.Wait(); err != nil {
		t.Fatalf("tar: %v", err)
	}
	main, err := os.ReadFile("testdata/legacyhost_main.go.txt")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(src, "cmd", "legacyhost")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), main, 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "legacyhost")
	build := exec.Command("go", "build", "-o", bin, "./cmd/legacyhost")
	build.Dir = src
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the 040 library host: %v\n%s", err, out)
	}
	return bin
}

// G3: a real 040 library host (the unmodified 040 scheduler, store and
// DispatchExecutor, built from the pinned commit) is killed while its worker
// runs. 040 would run that job again on its next start; after the 041
// migration the runner must not. The job is quarantined and the worker is left
// alone. This is a reconstruction through the 040 library, not a recorded 040
// binary: 040 shipped no daemon of its own.
func TestCLILegacyLibraryHostJobIsNeverRunAgain(t *testing.T) {
	host := buildLegacyHost(t)
	r := newRecNoCgroup(t)
	for _, f := range []string{r.path, r.path + "-wal", r.path + "-shm"} {
		_ = os.Remove(f)
	}
	work := r.e.cwd

	cmd := exec.Command(host, "run", r.path, r.fake, work)
	cmd.Dir = work
	cmd.Env = append(os.Environ(), "FAKE_LOG="+filepath.Join(work, "legacy.fake.log"))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	line, _ := bufio.NewReader(stdout).ReadString('\n')
	if strings.TrimSpace(line) != "started" {
		t.Fatalf("the 040 host did not start its worker: %q %s", line, stderr.String())
	}
	_ = syscall.Kill(cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Wait()

	var did string
	sq, err := sql.Open("sqlite", r.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := sq.QueryRow(`SELECT id FROM dispatches LIMIT 1`).Scan(&did); err != nil {
		t.Fatalf("the 040 host recorded no dispatch: %v", err)
	}
	sq.Close()
	t.Cleanup(func() { killCarriers(t, did) })
	if len(carriers(did)) == 0 {
		t.Fatal("the legacy worker did not survive its host")
	}

	// Control: the 040 library would run the job again on its next start.
	recover := exec.Command(host, "recover", r.path)
	recover.Dir = work
	rout, err := recover.CombinedOutput()
	if err != nil || !strings.Contains(string(rout), "legacy-job pending") {
		t.Fatalf("control: 040 RecoverPending = %q (%v), want it to re-queue legacy-job", rout, err)
	}

	d, err := db.Open(r.path, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	_ = d.Close()

	p := r.run(nil)
	p.waitReady(t)
	if _, se, code := r.e.ic("--db=q.db", "scheduler", "resume"); code != 0 {
		t.Fatalf("resume exit %d: %s", code, se)
	}
	for i := 0; i < 5; i++ {
		time.Sleep(200 * time.Millisecond)
	}
	r.stop(p)

	if st, oc := r.e.jobOutcome(r.path, "legacy-job"); st != "quarantined" || oc != "legacy_unknown" {
		t.Errorf("legacy-job = %s %s, want quarantined legacy_unknown", st, oc)
	}
	if n := r.execs(); n != 0 {
		t.Errorf("the executor ran %d times after the migration", n)
	}
	if n := r.e.count(r.path, `SELECT COUNT(*) FROM scheduler_job_attempts WHERE legacy = 0`); n != 0 {
		t.Errorf("%d non-legacy attempts exist", n)
	}
	if len(carriers(did)) == 0 {
		t.Error("the legacy worker was signalled")
	}
}
