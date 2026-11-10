package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// legacySchedulerJobsDDL is the 040 shape of scheduler_jobs, hard-coded so
// the test does not depend on schema.sql (which now carries the 041 shape).
const legacySchedulerJobsDDL = `
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

// make040DB returns a DB at schema version 40 with the legacy scheduler_jobs
// shape and none of the 041 objects.
func make040DB(t *testing.T) (*DB, string) {
	t.Helper()
	d, path := tempDB(t)
	ctx := context.Background()
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	for _, stmt := range []string{
		"DROP TABLE IF EXISTS scheduler_job_attempts",
		"DROP TABLE IF EXISTS scheduler_runner",
		"DROP TABLE IF EXISTS scheduler_jobs",
		"DELETE FROM state WHERE key = 'scheduler_paused'",
		legacySchedulerJobsDDL,
		"PRAGMA user_version = 40",
	} {
		if _, err := d.db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("make040DB %q: %v", stmt, err)
		}
	}
	// The setup Migrate's own backup is not under test.
	old, _ := filepath.Glob(path + ".backup-*")
	for _, f := range old {
		os.Remove(f)
	}
	return d, path
}

type legacyRow struct {
	id         string
	status     string
	dispatchID string // "" = NULL
	dispatch   string // dispatch status to create; "" = no dispatches row
	startedAt  int64  // 0 = NULL
	retryCount int
	opts       string
}

const goodOpts = `{"ProjectDir":"/work/proj","OutputFile":"/work/out/a","DispatchSH":"/opt/dispatch.sh"}`

// legacyDispositions covers every legacy disposition of plan section 3.
func legacyDispositions() []legacyRow {
	return []legacyRow{
		{id: "p-null", status: "pending", opts: goodOpts},
		{id: "p-null-started", status: "pending", startedAt: 1700000100, retryCount: 1, opts: goodOpts},
		{id: "p-term", status: "pending", dispatchID: "d-p-term", dispatch: "completed", opts: goodOpts},
		{id: "p-live", status: "pending", dispatchID: "d-p-live", dispatch: "running", opts: goodOpts},
		{id: "s-null", status: "scheduled", opts: goodOpts},
		{id: "r-null", status: "running", opts: goodOpts},
		{id: "rt-null", status: "retrying", opts: goodOpts},
		{id: "s-set", status: "scheduled", dispatchID: "d-s-set", dispatch: "spawned", startedAt: 1700000200, opts: goodOpts},
		{id: "r-set", status: "running", dispatchID: "d-r-set", dispatch: "running", startedAt: 1700000300, opts: goodOpts},
		{id: "rt-set", status: "retrying", dispatchID: "d-rt-set", dispatch: "failed", opts: goodOpts},
		{id: "r-gone", status: "running", dispatchID: "d-gone", dispatch: "", startedAt: 1700000400, opts: goodOpts},
		{id: "c-done", status: "completed", dispatchID: "d-c-done", dispatch: "completed", opts: goodOpts},
		{id: "f-fail", status: "failed", opts: goodOpts},
		{id: "x-canc", status: "cancelled", opts: goodOpts},
		{id: "p-badopts", status: "pending", dispatchID: "d-p-bad", dispatch: "completed", opts: `not json`},
		{id: "p-relopts", status: "pending", dispatchID: "d-p-rel", dispatch: "completed", opts: `{"ProjectDir":"rel/dir","OutputFile":"rel/out","max_retries":9}`},
	}
}

func seedLegacy(t *testing.T, d *DB, rows []legacyRow) {
	t.Helper()
	// A dangling dispatch_id can exist in a legacy DB whose FK was once
	// unenforced; reproduce it by disabling enforcement for the seed only.
	mustExec(t, d.db, "PRAGMA foreign_keys = OFF")
	defer mustExec(t, d.db, "PRAGMA foreign_keys = ON")
	for i, r := range rows {
		created := int64(1700000000 + i)
		if r.dispatch != "" {
			mustExec(t, d.db, `INSERT INTO dispatches (id, project_dir, status, created_at) VALUES (?, '/work/proj', ?, ?)`,
				r.dispatchID, r.dispatch, created+50)
		}
		var disp, started any
		if r.dispatchID != "" {
			disp = r.dispatchID
		}
		if r.startedAt != 0 {
			started = r.startedAt
		}
		var completed any
		if r.status == "completed" || r.status == "failed" || r.status == "cancelled" {
			completed = created + 900
		}
		mustExec(t, d.db, `INSERT INTO scheduler_jobs (id, status, agent_type, spawn_opts, max_retries, retry_count, dispatch_id, created_at, started_at, completed_at)
			VALUES (?, ?, 'codex', ?, 3, ?, ?, ?, ?, ?)`,
			r.id, r.status, r.opts, r.retryCount, disp, created, started, completed)
	}
}

type migrated struct {
	status, outcome, token                          sql.NullString
	leaseEpoch, attemptSeq, updatedAt               sql.NullInt64
	dispatchID, projectDir, outputBase, retryPolicy sql.NullString
}

func readJob(t *testing.T, d *DB, id string) migrated {
	t.Helper()
	var m migrated
	err := d.db.QueryRow(`SELECT status, outcome_class, claim_token, lease_epoch, attempt_seq, updated_at,
		dispatch_id, project_dir, output_base, retry_policy FROM scheduler_jobs WHERE id = ?`, id).
		Scan(&m.status, &m.outcome, &m.token, &m.leaseEpoch, &m.attemptSeq, &m.updatedAt,
			&m.dispatchID, &m.projectDir, &m.outputBase, &m.retryPolicy)
	if err != nil {
		t.Fatalf("read job %s: %v", id, err)
	}
	return m
}

func count(t *testing.T, db *sql.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", q, err)
	}
	return n
}

func str(t *testing.T, db *sql.DB, q string, args ...any) string {
	t.Helper()
	var s sql.NullString
	if err := db.QueryRow(q, args...).Scan(&s); err != nil {
		t.Fatalf("str %q: %v", q, err)
	}
	return s.String
}

// assertDispositions checks every legacy disposition after migration 041.
func assertDispositions(t *testing.T, d *DB) {
	t.Helper()
	type want struct {
		status, outcome string
		attempt         bool   // synthetic attempt expected
		dispatch        string // expected scheduler_jobs.dispatch_id ("" = NULL)
	}
	wants := map[string]want{
		"p-null":         {status: "quarantined", outcome: "legacy_unknown"},
		"p-null-started": {status: "quarantined", outcome: "legacy_unknown"},
		"p-term":         {status: "running", attempt: true, dispatch: "d-p-term"},
		"p-live":         {status: "running", attempt: true, dispatch: "d-p-live"},
		"s-null":         {status: "quarantined", outcome: "legacy_unknown"},
		"r-null":         {status: "quarantined", outcome: "legacy_unknown"},
		"rt-null":        {status: "quarantined", outcome: "legacy_unknown"},
		"s-set":          {status: "running", attempt: true, dispatch: "d-s-set"},
		"r-set":          {status: "running", attempt: true, dispatch: "d-r-set"},
		"rt-set":         {status: "running", attempt: true, dispatch: "d-rt-set"},
		"r-gone":         {status: "running", attempt: true, dispatch: "d-gone"},
		"c-done":         {status: "completed", dispatch: "d-c-done"},
		"f-fail":         {status: "failed"},
		"x-canc":         {status: "cancelled"},
		"p-badopts":      {status: "running", attempt: true, dispatch: "d-p-bad"},
		"p-relopts":      {status: "running", attempt: true, dispatch: "d-p-rel"},
	}
	for id, w := range wants {
		m := readJob(t, d, id)
		if m.status.String != w.status {
			t.Errorf("%s: status = %q, want %q", id, m.status.String, w.status)
		}
		if m.outcome.String != w.outcome {
			t.Errorf("%s: outcome_class = %q, want %q", id, m.outcome.String, w.outcome)
		}
		if m.dispatchID.String != w.dispatch {
			t.Errorf("%s: dispatch_id = %q, want %q", id, m.dispatchID.String, w.dispatch)
		}
		if !m.updatedAt.Valid || m.updatedAt.Int64 == 0 {
			t.Errorf("%s: updated_at not stamped", id)
		}
		nAttempts := count(t, d.db, "SELECT COUNT(*) FROM scheduler_job_attempts WHERE job_id = ?", id)
		if w.attempt {
			if nAttempts != 1 {
				t.Errorf("%s: %d synthetic attempts, want 1", id, nAttempts)
				continue
			}
			if m.token.String != "legacy-"+id || m.leaseEpoch.Int64 != 0 || !m.leaseEpoch.Valid || m.attemptSeq.Int64 != 1 {
				t.Errorf("%s: token=%q epoch=%v seq=%v, want legacy token, epoch 0, seq 1", id, m.token.String, m.leaseEpoch, m.attemptSeq)
			}
			var (
				no, epoch, counts, legacy int
				token, containment        string
				linked, claimed           sql.NullInt64
				attemptDispatch           sql.NullString
			)
			err := d.db.QueryRow(`SELECT attempt_no, claim_token, lease_epoch, dispatch_id, linked_at, claimed_at, containment, counts_toward_limit, legacy
				FROM scheduler_job_attempts WHERE job_id = ?`, id).
				Scan(&no, &token, &epoch, &attemptDispatch, &linked, &claimed, &containment, &counts, &legacy)
			if err != nil {
				t.Fatalf("%s: read attempt: %v", id, err)
			}
			if no != 1 || token != "legacy-"+id || epoch != 0 || containment != "none" || counts != 1 || legacy != 1 {
				t.Errorf("%s: attempt = no %d token %q epoch %d containment %q counts %d legacy %d", id, no, token, epoch, containment, counts, legacy)
			}
			if !linked.Valid || !claimed.Valid {
				t.Errorf("%s: attempt linked_at/claimed_at must be set (linked=%v claimed=%v)", id, linked, claimed)
			}
			wantAttemptDispatch := w.dispatch
			if id == "r-gone" {
				wantAttemptDispatch = "" // the dispatch row is gone: no dangling FK
			}
			if attemptDispatch.String != wantAttemptDispatch {
				t.Errorf("%s: attempt dispatch_id = %q, want %q", id, attemptDispatch.String, wantAttemptDispatch)
			}
		} else {
			if nAttempts != 0 {
				t.Errorf("%s: %d attempts synthesized, want none", id, nAttempts)
			}
			if m.token.Valid {
				t.Errorf("%s: claim_token = %q, want NULL", id, m.token.String)
			}
		}
	}

	// No row may be left scheduled, retrying or pending-and-runnable.
	for _, s := range []string{"scheduled", "retrying", "pending"} {
		if n := count(t, d.db, "SELECT COUNT(*) FROM scheduler_jobs WHERE status = ?", s); n != 0 {
			t.Errorf("%d rows left %q after migration", n, s)
		}
	}

	// linked_at and claimed_at come from the job: started_at, else created_at
	// (the dispatches table is not consulted, so very old shapes still migrate).
	if got := count(t, d.db, "SELECT linked_at FROM scheduler_job_attempts WHERE job_id = 'p-term'"); got != 1700000002 {
		t.Errorf("p-term linked_at = %d, want the job created_at 1700000002", got)
	}
	if got := count(t, d.db, "SELECT linked_at FROM scheduler_job_attempts WHERE job_id = 'r-gone'"); got != 1700000400 {
		t.Errorf("r-gone linked_at = %d, want started_at 1700000400", got)
	}

	// Backfill from spawn_opts: absolute values only.
	if m := readJob(t, d, "p-term"); m.projectDir.String != "/work/proj" || m.outputBase.String != "/work/out/a" {
		t.Errorf("p-term backfill project_dir=%q output_base=%q", m.projectDir.String, m.outputBase.String)
	}
	for _, id := range []string{"p-badopts", "p-relopts"} {
		if m := readJob(t, d, id); m.projectDir.Valid || m.outputBase.Valid {
			t.Errorf("%s: backfilled a non-absolute or invalid value: project_dir=%v output_base=%v", id, m.projectDir, m.outputBase)
		}
	}
	// max_attempts is capped at 1 for every legacy row (no idempotency flag),
	// whatever max_retries was.
	for id := range wants {
		m := readJob(t, d, id)
		if got := str(t, d.db, "SELECT json_extract(retry_policy, '$.max_attempts') FROM scheduler_jobs WHERE id = ?", id); got != "1" {
			t.Errorf("%s: retry_policy.max_attempts = %q, want 1 (policy %q)", id, got, m.retryPolicy.String)
		}
	}
	// The executor stays in spawn_opts; the migration does not copy it.
	if got := str(t, d.db, "SELECT json_extract(spawn_opts, '$.DispatchSH') FROM scheduler_jobs WHERE id = 'p-term'"); got != "/opt/dispatch.sh" {
		t.Errorf("spawn_opts DispatchSH = %q", got)
	}
}

var hex32 = regexp.MustCompile(`^[0-9a-f]{32}$`)

func assertRunnerSeed(t *testing.T, d *DB) string {
	t.Helper()
	if n := count(t, d.db, "SELECT COUNT(*) FROM scheduler_runner"); n != 1 {
		t.Fatalf("scheduler_runner rows = %d, want 1", n)
	}
	var (
		id, epoch    int
		queueID, st  string
		runnerID, pn sql.NullString
	)
	if err := d.db.QueryRow("SELECT id, epoch, queue_id, state, runner_id, host FROM scheduler_runner").
		Scan(&id, &epoch, &queueID, &st, &runnerID, &pn); err != nil {
		t.Fatal(err)
	}
	if id != 1 || epoch != 1 || st != "stopped" || runnerID.Valid {
		t.Errorf("runner seed = id %d epoch %d state %q runner_id %v", id, epoch, st, runnerID)
	}
	if !hex32.MatchString(queueID) {
		t.Errorf("queue_id %q is not 128 random bits as lower hex", queueID)
	}
	return queueID
}

func isPaused(t *testing.T, d *DB) bool {
	t.Helper()
	var payload sql.NullString
	err := d.db.QueryRow("SELECT payload FROM state WHERE key='scheduler_paused' AND scope_id='global'").Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return payload.String == "true"
}

type fkInfo struct{ table, from, to, onUpdate, onDelete string }

func foreignKeys(t *testing.T, d *DB, table string) map[string]fkInfo {
	t.Helper()
	rows, err := d.db.Query(fmt.Sprintf("PRAGMA foreign_key_list(%s)", table))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]fkInfo{}
	for rows.Next() {
		var id, seq int
		var f fkInfo
		var match string
		if err := rows.Scan(&id, &seq, &f.table, &f.from, &f.to, &f.onUpdate, &f.onDelete, &match); err != nil {
			t.Fatal(err)
		}
		out[f.from] = f
	}
	return out
}

func assertShape041(t *testing.T, d *DB) {
	t.Helper()
	fk := foreignKeys(t, d, "scheduler_job_attempts")
	if f := fk["job_id"]; f.table != "scheduler_jobs" || f.onDelete != "CASCADE" {
		t.Errorf("attempts.job_id FK = %+v, want scheduler_jobs ON DELETE CASCADE", f)
	}
	if f := fk["dispatch_id"]; f.table != "dispatches" || f.onDelete != "SET NULL" {
		t.Errorf("attempts.dispatch_id FK = %+v, want dispatches ON DELETE SET NULL", f)
	}
	// The legacy FK keeps no ON DELETE clause (a table rebuild is out of scope).
	if f := foreignKeys(t, d, "scheduler_jobs")["dispatch_id"]; f.table != "dispatches" || f.onDelete != "NO ACTION" {
		t.Errorf("scheduler_jobs.dispatch_id FK = %+v, want dispatches NO ACTION", f)
	}
	for _, idx := range []string{"idx_scheduler_jobs_idempotency", "idx_scheduler_jobs_claim"} {
		if n := count(t, d.db, "SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?", idx); n != 1 {
			t.Errorf("index %s missing", idx)
		}
	}
	// The idempotency index is UNIQUE and partial.
	sqlText := str(t, d.db, "SELECT sql FROM sqlite_master WHERE name='idx_scheduler_jobs_idempotency'")
	if !strings.Contains(sqlText, "UNIQUE") || !strings.Contains(strings.ToUpper(sqlText), "WHERE IDEMPOTENCY_KEY IS NOT NULL") {
		t.Errorf("idempotency index is not a partial UNIQUE index: %s", sqlText)
	}
	claimCols := str(t, d.db, `SELECT group_concat(name, ',') FROM (SELECT name FROM pragma_index_info('idx_scheduler_jobs_claim') ORDER BY seqno)`)
	if claimCols != "status,not_before,priority,created_at" {
		t.Errorf("claim index columns = %q", claimCols)
	}
	if v := str(t, d.db, "PRAGMA user_version"); v != "41" {
		t.Errorf("user_version = %s, want 41", v)
	}
	// claim_token is unique across attempts.
	mustExec(t, d.db, `INSERT INTO scheduler_jobs (id, status, spawn_opts, created_at, updated_at) VALUES ('shape-a','running','{}',1,1),('shape-b','running','{}',1,1)`)
	mustExec(t, d.db, `INSERT INTO scheduler_job_attempts (job_id, attempt_no, claim_token, claimed_at) VALUES ('shape-a',1,'tok-shape',1)`)
	if _, err := d.db.Exec(`INSERT INTO scheduler_job_attempts (job_id, attempt_no, claim_token, claimed_at) VALUES ('shape-b',1,'tok-shape',1)`); err == nil {
		t.Error("duplicate claim_token accepted")
	}
	if _, err := d.db.Exec(`INSERT INTO scheduler_job_attempts (job_id, attempt_no, claim_token, claimed_at) VALUES ('shape-a',1,'tok-other',1)`); err == nil {
		t.Error("duplicate (job_id, attempt_no) accepted")
	}
	// Idempotency key uniqueness (partial: NULLs never collide).
	mustExec(t, d.db, `UPDATE scheduler_jobs SET idempotency_key='k1' WHERE id='shape-a'`)
	if _, err := d.db.Exec(`UPDATE scheduler_jobs SET idempotency_key='k1' WHERE id='shape-b'`); err == nil {
		t.Error("duplicate idempotency_key accepted")
	}
	mustExec(t, d.db, `DELETE FROM scheduler_jobs WHERE id IN ('shape-a','shape-b')`)
	if n := count(t, d.db, "SELECT COUNT(*) FROM scheduler_job_attempts WHERE job_id LIKE 'shape-%'"); n != 0 {
		t.Errorf("deleting a job left %d attempts (ON DELETE CASCADE)", n)
	}
}

var upgradePaths = map[string]func(t *testing.T, d *DB){
	"Migrate": func(t *testing.T, d *DB) {
		if err := d.Migrate(context.Background()); err != nil {
			t.Fatalf("Migrate: %v", err)
		}
	},
	"Migrator": func(t *testing.T, d *DB) {
		m, err := NewMigrator(d)
		if err != nil {
			t.Fatal(err)
		}
		n, err := m.Run(context.Background())
		if err != nil {
			t.Fatalf("Migrator.Run: %v", err)
		}
		if n != 1 {
			t.Fatalf("Migrator applied %d migrations, want 1 (041)", n)
		}
	},
}

// Unit test 1: migration 041 from a 040 DB seeded with every legacy disposition.
func TestMigration041LegacyDispositions(t *testing.T) {
	for name, upgrade := range upgradePaths {
		t.Run(name, func(t *testing.T) {
			d, _ := make040DB(t)
			seedLegacy(t, d, legacyDispositions())
			upgrade(t, d)

			assertDispositions(t, d)
			assertRunnerSeed(t, d)
			if !isPaused(t, d) {
				t.Error("queue must be left paused when legacy rows are running or quarantined")
			}
			assertShape041(t, d)

			// Running the upgrade a second time changes nothing.
			before := str(t, d.db, `SELECT group_concat(id||status||COALESCE(claim_token,'')||updated_at, '|') FROM (SELECT * FROM scheduler_jobs ORDER BY id)`)
			mustExec(t, d.db, "PRAGMA user_version = 40")
			if name == "Migrate" {
				upgrade(t, d)
			} else {
				m, _ := NewMigrator(d)
				if _, err := m.Run(context.Background()); err != nil {
					t.Fatalf("second Migrator.Run: %v", err)
				}
			}
			after := str(t, d.db, `SELECT group_concat(id||status||COALESCE(claim_token,'')||updated_at, '|') FROM (SELECT * FROM scheduler_jobs ORDER BY id)`)
			if before != after {
				t.Errorf("re-applying 041 changed rows:\nbefore %s\nafter  %s", before, after)
			}
			if n := count(t, d.db, "SELECT COUNT(*) FROM scheduler_job_attempts"); n != 8 {
				t.Errorf("attempts after re-apply = %d, want 8", n)
			}
		})
	}
}

func TestMigration041TerminalOnlyIsNotPaused(t *testing.T) {
	for name, upgrade := range upgradePaths {
		t.Run(name, func(t *testing.T) {
			d, _ := make040DB(t)
			seedLegacy(t, d, []legacyRow{
				{id: "c-done", status: "completed", dispatchID: "d-c", dispatch: "completed", opts: goodOpts},
				{id: "f-fail", status: "failed", opts: goodOpts},
			})
			// An old explicit resume must survive a terminal-only upgrade.
			mustExec(t, d.db, `INSERT INTO state (key, scope_id, payload) VALUES ('scheduler_paused','global','false')`)
			upgrade(t, d)
			if isPaused(t, d) {
				t.Error("a queue with only terminal rows must not be paused")
			}
			assertRunnerSeed(t, d)
			if n := count(t, d.db, "SELECT COUNT(*) FROM scheduler_job_attempts"); n != 0 {
				t.Errorf("attempts = %d, want 0", n)
			}
		})
	}
}

// A legacy explicit resume ('false') is overridden when legacy rows remain.
func TestMigration041PauseOverridesResume(t *testing.T) {
	d, _ := make040DB(t)
	seedLegacy(t, d, []legacyRow{{id: "p-null", status: "pending", opts: goodOpts}})
	mustExec(t, d.db, `INSERT INTO state (key, scope_id, payload) VALUES ('scheduler_paused','global','false')`)
	upgradePaths["Migrate"](t, d)
	if !isPaused(t, d) {
		t.Error("legacy quarantined row must pause a previously resumed queue")
	}
}

func tableColumns(t *testing.T, d *DB, table string) []string {
	t.Helper()
	rows, err := d.db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, fmt.Sprintf("%s|%s|%d|%s|%d", name, typ, notnull, dflt.String, pk))
	}
	sort.Strings(cols)
	return cols
}

func indexNames(t *testing.T, d *DB, table string) []string {
	t.Helper()
	rows, err := d.db.Query(`SELECT name FROM sqlite_master WHERE type='index' AND tbl_name=? AND name NOT LIKE 'sqlite_autoindex%' ORDER BY name`, table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

// DB.Migrate (inline stage + schema.sql) and the file Migrator must agree
// with each other and with a fresh install.
func TestMigration041Parity(t *testing.T) {
	fresh, _ := tempDB(t)
	if err := fresh.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	viaMigrate, _ := make040DB(t)
	upgradePaths["Migrate"](t, viaMigrate)
	viaMigrator, _ := make040DB(t)
	upgradePaths["Migrator"](t, viaMigrator)

	for _, table := range []string{"scheduler_jobs", "scheduler_runner", "scheduler_job_attempts"} {
		want := tableColumns(t, fresh, table)
		if len(want) == 0 {
			t.Fatalf("fresh install has no %s", table)
		}
		for name, d := range map[string]*DB{"Migrate": viaMigrate, "Migrator": viaMigrator} {
			if got := tableColumns(t, d, table); strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("%s via %s differs from a fresh install:\n got %v\nwant %v", table, name, got, want)
			}
			if got, w := indexNames(t, d, table), indexNames(t, fresh, table); strings.Join(got, ",") != strings.Join(w, ",") {
				t.Errorf("%s indexes via %s = %v, fresh = %v", table, name, got, w)
			}
		}
	}
	assertShape041(t, fresh)
	assertRunnerSeed(t, fresh)
	if isPaused(t, fresh) {
		t.Error("a fresh install must not be paused")
	}
}

func backupsOf(t *testing.T, path string) []string {
	t.Helper()
	matches, err := filepath.Glob(path + ".backup-*")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(matches)
	return matches
}

func openRO(t *testing.T, path string) *sql.DB {
	t.Helper()
	dsn := "file:" + (&url.URL{Path: path}).EscapedPath() + "?mode=ro"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

var backupName = regexp.MustCompile(`\.backup-(\d+)-\d{8}-\d{6}(-\d+)?\.db$`)

// Unit test 1b: the migrator's backup is a verified VACUUM INTO snapshot that
// includes rows that exist only in the WAL.
func TestMigrateBackupIsWALSafe(t *testing.T) {
	d, path := make040DB(t)
	ctx := context.Background()
	mustExec(t, d.db, "PRAGMA wal_autocheckpoint = 0")
	mustExec(t, d.db, "CREATE TABLE wal_probe (n INTEGER)")

	// A reader on a second connection pins the WAL so nothing is checkpointed.
	reader, err := sql.Open("sqlite", "file:"+(&url.URL{Path: path}).EscapedPath())
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	rtx, err := reader.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer rtx.Rollback()
	var probe int
	if err := rtx.QueryRow("SELECT COUNT(*) FROM wal_probe").Scan(&probe); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 50; i++ {
		mustExec(t, d.db, "INSERT INTO wal_probe VALUES (?)", i)
	}
	seedLegacy(t, d, legacyDispositions())
	if info, err := os.Stat(path + "-wal"); err != nil || info.Size() == 0 {
		t.Fatalf("rows are not WAL-only (wal stat %v, err %v)", info, err)
	}
	// The main file alone lacks the rows (the failure mode of a raw copy).
	raw := filepath.Join(t.TempDir(), "raw.db")
	if err := copyFile(path, raw); err != nil {
		t.Fatal(err)
	}
	if n := count(t, openRO(t, raw), "SELECT COUNT(*) FROM sqlite_master WHERE name='wal_probe'"); n != 0 {
		t.Skipf("raw copy already contains the WAL-only table; the probe cannot discriminate")
	}

	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	backups := backupsOf(t, path)
	if len(backups) != 1 {
		t.Fatalf("backups = %v, want exactly one", backups)
	}
	m := backupName.FindStringSubmatch(backups[0])
	if m == nil || m[1] != "40" {
		t.Fatalf("backup name %q does not carry schema version 40", backups[0])
	}
	b := openRO(t, backups[0])
	if got := str(t, b, "PRAGMA integrity_check"); got != "ok" {
		t.Errorf("integrity_check = %q", got)
	}
	if got := str(t, b, "PRAGMA user_version"); got != "40" {
		t.Errorf("backup user_version = %q, want 40", got)
	}
	if n := count(t, b, "SELECT COUNT(*) FROM wal_probe"); n != 50 {
		t.Errorf("backup wal_probe rows = %d, want 50 (WAL content lost)", n)
	}
	if n := count(t, b, "SELECT COUNT(*) FROM scheduler_jobs"); n != len(legacyDispositions()) {
		t.Errorf("backup scheduler_jobs rows = %d, want %d", n, len(legacyDispositions()))
	}
	if n := count(t, b, "SELECT COUNT(*) FROM sqlite_master WHERE name='scheduler_runner'"); n != 0 {
		t.Error("the backup must be the pre-migration state")
	}
	// The live DB migrated.
	if got := str(t, d.db, "PRAGMA user_version"); got != "41" {
		t.Errorf("live user_version = %q, want 41", got)
	}
}

func TestMigrateBackupFailureLeavesDBUnmigrated(t *testing.T) {
	cases := map[string]func(t *testing.T){
		"backup fails": func(t *testing.T) {
			old := migrateBackupFn
			migrateBackupFn = func(ctx context.Context, d *DB, target string) error { return errors.New("injected backup failure") }
			t.Cleanup(func() { migrateBackupFn = old })
		},
		"too little free space": func(t *testing.T) {
			old := diskSpaceCheck
			diskSpaceCheck = func(dir string, minBytes uint64) error { return errors.New("injected: disk full") }
			t.Cleanup(func() { diskSpaceCheck = old })
		},
		"backup fails verification": func(t *testing.T) {
			old := migrateBackupFn
			migrateBackupFn = func(ctx context.Context, d *DB, target string) error {
				// Produce a readable file with the wrong user_version.
				if err := old(ctx, d, target); err != nil {
					return err
				}
				bdb, err := sql.Open("sqlite", "file:"+(&url.URL{Path: target}).EscapedPath())
				if err != nil {
					return err
				}
				defer bdb.Close()
				_, err = bdb.Exec("PRAGMA user_version = 7")
				return err
			}
			t.Cleanup(func() { migrateBackupFn = old })
		},
	}
	for name, inject := range cases {
		t.Run(name, func(t *testing.T) {
			d, path := make040DB(t)
			seedLegacy(t, d, legacyDispositions())
			before := count(t, d.db, "SELECT COUNT(*) FROM scheduler_jobs")
			inject(t)
			if err := d.Migrate(context.Background()); err == nil {
				t.Fatal("Migrate succeeded despite a backup failure")
			}
			if got := str(t, d.db, "PRAGMA user_version"); got != "40" {
				t.Errorf("user_version = %q, want 40 (DB must stay untouched)", got)
			}
			if n := count(t, d.db, "SELECT COUNT(*) FROM sqlite_master WHERE name IN ('scheduler_runner','scheduler_job_attempts')"); n != 0 {
				t.Errorf("041 objects were created despite the failed backup")
			}
			if n := count(t, d.db, "SELECT COUNT(*) FROM scheduler_jobs"); n != before {
				t.Errorf("scheduler_jobs rows changed: %d -> %d", before, n)
			}
			if n := count(t, d.db, "SELECT COUNT(*) FROM pragma_table_info('scheduler_jobs') WHERE name='claim_token'"); n != 0 {
				t.Error("041 columns were added despite the failed backup")
			}
			// A failed or unverified backup must not be left looking valid.
			if b := backupsOf(t, path); len(b) != 0 {
				t.Errorf("leftover backup files after failure: %v", b)
			}
		})
	}
}

func TestMigrateCurrentDBMakesNoBackup(t *testing.T) {
	d, path := tempDB(t)
	ctx := context.Background()
	if err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	before := len(backupsOf(t, path))
	if err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if after := len(backupsOf(t, path)); after != before {
		t.Errorf("re-migrating a current DB created %d new backups", after-before)
	}
}

// Unit test 1c: restore the migrator's actual 040 backup over the 041 DB.
func TestMigration041FirstUpgradeRestore(t *testing.T) {
	run := func(t *testing.T, rows []legacyRow, wantPaused bool) {
		d, path := make040DB(t)
		seedLegacy(t, d, rows)
		ctx := context.Background()
		if err := d.Migrate(ctx); err != nil {
			t.Fatalf("Migrate: %v", err)
		}
		oldQueue := assertRunnerSeed(t, d)
		backups := backupsOf(t, path)
		if len(backups) != 1 {
			t.Fatalf("backups = %v", backups)
		}
		// Offline restore: every handle closed, WAL and SHM removed.
		d.Close()
		for _, suffix := range []string{"-wal", "-shm"} {
			os.Remove(path + suffix)
		}
		if err := copyFile(backups[0], path); err != nil {
			t.Fatal(err)
		}
		d2, err := Open(path, 100)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		defer d2.Close()
		if got := str(t, d2.db, "PRAGMA user_version"); got != "40" {
			t.Fatalf("restored user_version = %q, want 40", got)
		}
		if n := count(t, d2.db, "SELECT COUNT(*) FROM sqlite_master WHERE name='scheduler_runner'"); n != 0 {
			t.Fatal("a 040 backup must have no scheduler_runner")
		}
		if err := d2.Migrate(ctx); err != nil {
			t.Fatalf("re-apply 041: %v", err)
		}
		newQueue := assertRunnerSeed(t, d2)
		if newQueue == oldQueue {
			t.Errorf("queue_id %s was reused after restoring a 040 backup", newQueue)
		}
		if n := count(t, d2.db, "SELECT COUNT(*) FROM scheduler_jobs WHERE status IN ('pending','scheduled','retrying')"); n != 0 {
			t.Errorf("%d rows are automatically runnable after the restore", n)
		}
		if got := isPaused(t, d2); got != wantPaused {
			t.Errorf("paused = %v, want %v", got, wantPaused)
		}
	}
	t.Run("legacy in-flight row pauses", func(t *testing.T) {
		run(t, []legacyRow{
			{id: "r-set", status: "running", dispatchID: "d-1", dispatch: "running", opts: goodOpts},
			{id: "p-null", status: "pending", opts: goodOpts},
			{id: "c-done", status: "completed", opts: goodOpts},
		}, true)
	})
	t.Run("terminal-only does not pause", func(t *testing.T) {
		run(t, []legacyRow{
			{id: "c-done", status: "completed", dispatchID: "d-2", dispatch: "completed", opts: goodOpts},
			{id: "f-fail", status: "failed", opts: goodOpts},
		}, false)
	})
}
