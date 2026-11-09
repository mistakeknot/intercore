package routing

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mistakeknot/intercore/internal/db"
)

var retNow = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

func retentionDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open(path, 5*time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	if err := d.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return d.SqlDB(), path
}

// seedAt inserts one decision decided daysAgo days before retNow. ctxBytes sizes
// context_json; extra columns are set so a restore round trip is checked on all of them.
func seedAt(t *testing.T, sdb *sql.DB, daysAgo float64, ctxBytes int, mod func(*RecordDecisionOpts)) int64 {
	t.Helper()
	o := RecordDecisionOpts{
		ProjectDir: "/proj", Agent: "fd-correctness", SelectedModel: "sonnet", RuleMatched: "default",
		Phase: "build", Category: "review", Candidates: `["a","b"]`, Excluded: `[]`, PolicyHash: "h1",
		Complexity: 3, ContextJSON: `{"pad":"` + strings.Repeat("x", ctxBytes) + `"}`,
		FloorApplied: true, FloorFrom: "haiku", FloorTo: "sonnet", BeadID: "b-1", SessionID: "s-1",
	}
	if mod != nil {
		mod(&o)
	}
	id, err := NewDecisionStore(sdb).Record(context.Background(), o)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	ts := retNow.Add(-time.Duration(daysAgo * 24 * float64(time.Hour))).Unix()
	if _, err := sdb.Exec(`UPDATE routing_decisions SET decided_at = ? WHERE id = ?`, ts, id); err != nil {
		t.Fatalf("set decided_at: %v", err)
	}
	return id
}

func countRows(t *testing.T, sdb *sql.DB) int64 {
	t.Helper()
	var n int64
	if err := sdb.QueryRow(`SELECT COUNT(*) FROM routing_decisions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func idsOf(t *testing.T, sdb *sql.DB) []int64 {
	t.Helper()
	rs, err := sdb.Query(`SELECT id FROM routing_decisions ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	var out []int64
	for rs.Next() {
		var id int64
		rs.Scan(&id)
		out = append(out, id)
	}
	return out
}

func TestPruneDecisionsArchivesAndDeletesOldRows(t *testing.T) {
	sdb, _ := retentionDB(t)
	dir := t.TempDir()
	var old, young []int64
	for _, d := range []float64{90, 60, 31, 30.5} {
		old = append(old, seedAt(t, sdb, d, 50, nil))
	}
	for _, d := range []float64{29, 5, 0.1} {
		young = append(young, seedAt(t, sdb, d, 50, nil))
	}
	res, err := PruneDecisions(context.Background(), sdb, PruneOpts{OlderThan: DefaultRetention, Now: retNow, ArchiveDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if res.Eligible != 4 || res.Archived != 4 || res.Deleted != 4 {
		t.Fatalf("result = %+v, want 4/4/4", res)
	}
	if got := idsOf(t, sdb); !reflect.DeepEqual(got, young) {
		t.Fatalf("remaining ids = %v, want %v", got, young)
	}
	if len(res.Files) == 0 {
		t.Fatal("no archive files reported")
	}
	for _, f := range res.Files {
		if st, err := os.Stat(f); err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("archive %s: err=%v mode=%v, want 0600", f, err, st)
		}
	}
}

func TestPruneDecisionsDryRunChangesNothing(t *testing.T) {
	sdb, _ := retentionDB(t)
	dir := filepath.Join(t.TempDir(), "archive")
	seedAt(t, sdb, 90, 10, nil)
	seedAt(t, sdb, 1, 10, nil)
	res, err := PruneDecisions(context.Background(), sdb, PruneOpts{OlderThan: DefaultRetention, Now: retNow, ArchiveDir: dir, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Eligible != 1 || res.Deleted != 0 || res.Archived != 0 || !res.DryRun {
		t.Fatalf("result = %+v", res)
	}
	if countRows(t, sdb) != 2 {
		t.Fatal("dry run deleted rows")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("dry run created the archive dir (err=%v)", err)
	}
}

func TestPruneDecisionsWithoutArchiveDeletes(t *testing.T) {
	sdb, _ := retentionDB(t)
	seedAt(t, sdb, 90, 10, nil)
	seedAt(t, sdb, 1, 10, nil)
	res, err := PruneDecisions(context.Background(), sdb, PruneOpts{OlderThan: DefaultRetention, Now: retNow})
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 1 || res.Archived != 0 || countRows(t, sdb) != 1 {
		t.Fatalf("result = %+v rows=%d", res, countRows(t, sdb))
	}
}

func TestPruneDecisionsRejectsNonPositiveWindow(t *testing.T) {
	sdb, _ := retentionDB(t)
	seedAt(t, sdb, 90, 10, nil)
	for _, w := range []time.Duration{0, -time.Hour} {
		if _, err := PruneDecisions(context.Background(), sdb, PruneOpts{OlderThan: w, Now: retNow}); err == nil {
			t.Fatalf("window %v accepted", w)
		}
	}
	if countRows(t, sdb) != 1 {
		t.Fatal("rows deleted by a rejected prune")
	}
}

func TestPruneDecisionsEmptyTableAndNothingOld(t *testing.T) {
	sdb, _ := retentionDB(t)
	res, err := PruneDecisions(context.Background(), sdb, PruneOpts{OlderThan: DefaultRetention, Now: retNow})
	if err != nil || res.Eligible != 0 {
		t.Fatalf("empty: res=%+v err=%v", res, err)
	}
	seedAt(t, sdb, 1, 10, nil)
	res, err = PruneDecisions(context.Background(), sdb, PruneOpts{OlderThan: DefaultRetention, Now: retNow})
	if err != nil || res.Eligible != 0 || countRows(t, sdb) != 1 {
		t.Fatalf("nothing old: res=%+v err=%v", res, err)
	}
}

func TestPruneDecisionsKeepsRowsStillReferenced(t *testing.T) {
	sdb, _ := retentionDB(t)
	ctx := context.Background()
	mustExec := func(q string, a ...any) {
		t.Helper()
		if _, err := sdb.ExecContext(ctx, q, a...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	mustExec(`INSERT INTO dispatches (id, project_dir, status) VALUES ('d-run','/p','running'), ('d-done','/p','completed')`)
	mustExec(`INSERT INTO runs (id, project_dir, goal, status) VALUES ('r-open','/p','g','active'), ('r-done','/p','g','completed')`)

	keepDispatch := seedAt(t, sdb, 90, 10, func(o *RecordDecisionOpts) { o.DispatchID = "d-run" })
	dropDispatch := seedAt(t, sdb, 90, 10, func(o *RecordDecisionOpts) { o.DispatchID = "d-done" })
	keepRun := seedAt(t, sdb, 90, 10, func(o *RecordDecisionOpts) { o.RunID = "r-open" })
	dropRun := seedAt(t, sdb, 90, 10, func(o *RecordDecisionOpts) { o.RunID = "r-done" })
	orphan := seedAt(t, sdb, 90, 10, func(o *RecordDecisionOpts) { o.DispatchID = "gone"; o.RunID = "gone" })
	termRef := seedAt(t, sdb, 90, 10, nil)
	mustExec(`INSERT INTO dispatch_terminals (dispatch_id, attempt, status, from_status, terminal_source, evidence_status, route_decision_id, event_id, created_at)
		VALUES ('d-done', 1, 'completed', 'running', 'legacy', 'unrecorded', ?, 1, 1)`, termRef)

	res, err := PruneDecisions(ctx, sdb, PruneOpts{OlderThan: DefaultRetention, Now: retNow, ArchiveDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	want := []int64{keepDispatch, keepRun, termRef}
	if got := idsOf(t, sdb); !reflect.DeepEqual(got, want) {
		t.Fatalf("remaining = %v, want %v (dropped dispatch %d, run %d, orphan %d)", got, want, dropDispatch, dropRun, orphan)
	}
	if res.Deleted != 3 || res.Protected != 3 {
		t.Fatalf("result = %+v, want deleted 3, protected 3", res)
	}
	// A dry run counts only what a real run could delete.
	res, err = PruneDecisions(ctx, sdb, PruneOpts{OlderThan: DefaultRetention, Now: retNow, DryRun: true})
	if err != nil || res.Eligible != 0 || res.Protected != 3 {
		t.Fatalf("second pass: %+v err=%v", res, err)
	}
}

// decided_at is not guaranteed monotonic in id (clock steps, imports). Every old
// row goes, wherever it sits, and no young row ever does: a young or future-dated
// row at a low id must not hold retention back.
func TestPruneDecisionsOutOfOrderTimestamps(t *testing.T) {
	sdb, _ := retentionDB(t)
	young0 := seedAt(t, sdb, -400, 10, nil) // dated far in the future, lowest id
	a := seedAt(t, sdb, 90, 10, nil)        // old
	b := seedAt(t, sdb, 2, 10, nil)         // young, in the middle of the old range
	c := seedAt(t, sdb, 80, 10, nil)        // old, after a young row
	d := seedAt(t, sdb, 1, 10, nil)         // young
	res, err := PruneDecisions(context.Background(), sdb, PruneOpts{OlderThan: DefaultRetention, Now: retNow})
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{young0, b, d}; !reflect.DeepEqual(idsOf(t, sdb), want) {
		t.Fatalf("remaining %v, want %v (old rows %d and %d should be gone)", idsOf(t, sdb), want, a, c)
	}
	if res.Deleted != 2 {
		t.Fatalf("deleted %d, want 2", res.Deleted)
	}
}

// Archives are written whole and renamed into place, and only the batch's rows
// are deleted after that: after a crash between archive and delete a rerun
// archives the same rows again under the same name, never into a file that
// already exists.
func TestPruneRerunAfterArchiveWithoutDeleteMakesNoDuplicateFile(t *testing.T) {
	sdb, _ := retentionDB(t)
	dir := t.TempDir()
	ctx := context.Background()
	seedAt(t, sdb, 60, 10, nil)
	seedAt(t, sdb, 61, 10, nil)
	// Simulate the crash: archive the old rows, delete nothing.
	old, err := selectOldBatch(ctx, sdb, 0, retNow.Add(-DefaultRetention).Unix(), 100)
	if err != nil || len(old) != 2 {
		t.Fatalf("select: %v %v", old, err)
	}
	var rows []Decision
	for _, o := range old {
		rows = append(rows, o.Decision)
	}
	first, err := writeArchive(dir, rows)
	if err != nil {
		t.Fatal(err)
	}
	res, err := PruneDecisions(ctx, sdb, PruneOpts{OlderThan: DefaultRetention, Now: retNow, ArchiveDir: dir})
	if err != nil || res.Deleted != 2 {
		t.Fatalf("rerun: %+v %v", res, err)
	}
	if !reflect.DeepEqual(first, res.Files) {
		t.Fatalf("rerun wrote %v, want the same file %v", res.Files, first)
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "*")); len(m) != 1 {
		t.Fatalf("archive dir holds %v, want one file and no temp files", m)
	}
}

// A row that came to be referenced after it was selected survives the delete.
func TestDeleteBatchKeepsRowThatBecameProtected(t *testing.T) {
	sdb, _ := retentionDB(t)
	ctx := context.Background()
	id := seedAt(t, sdb, 90, 10, func(o *RecordDecisionOpts) { o.DispatchID = "d-late" })
	other := seedAt(t, sdb, 90, 10, nil)
	old, err := selectOldBatch(ctx, sdb, 0, retNow.Add(-DefaultRetention).Unix(), 100)
	if err != nil || len(old) != 2 {
		t.Fatalf("select: %v %v", old, err)
	}
	if old[0].protected || old[1].protected {
		t.Fatal("nothing should be protected yet")
	}
	// The dispatch starts after the row was selected.
	if _, err := sdb.ExecContext(ctx, `INSERT INTO dispatches (id, project_dir, status) VALUES ('d-late','/p','running')`); err != nil {
		t.Fatal(err)
	}
	var rows []Decision
	for _, o := range old {
		rows = append(rows, o.Decision)
	}
	n, err := deleteBatch(ctx, sdb, rows)
	if err != nil || n != 1 {
		t.Fatalf("deleteBatch = %d, %v; want 1", n, err)
	}
	if got := idsOf(t, sdb); !reflect.DeepEqual(got, []int64{id}) {
		t.Fatalf("remaining %v, want only %d (%d should be deleted)", got, id, other)
	}
}

func TestRestoreRejectsDamagedArchiveAndChangesNothing(t *testing.T) {
	sdb, _ := retentionDB(t)
	dir := t.TempDir()
	ctx := context.Background()
	seedAt(t, sdb, 60, 10, nil)
	seedAt(t, sdb, 61, 10, nil)
	res, err := PruneDecisions(ctx, sdb, PruneOpts{OlderThan: DefaultRetention, Now: retNow, ArchiveDir: dir})
	if err != nil || len(res.Files) != 1 {
		t.Fatalf("prune: %+v %v", res, err)
	}
	good, _ := os.ReadFile(res.Files[0])
	bad := filepath.Join(dir, "routing-decisions-2026-01-9-9-deadbeef.jsonl.gz")
	if err := os.WriteFile(bad, good[:len(good)/2], 0o600); err != nil { // truncated
		t.Fatal(err)
	}
	_, err = RestoreDecisions(ctx, sdb, []string{dir})
	if err == nil || !strings.Contains(err.Error(), "deadbeef") {
		t.Fatalf("restore err = %v, want one naming the damaged file", err)
	}
	if countRows(t, sdb) != 0 {
		t.Fatal("a failed restore changed the table")
	}
	// Without the damaged file the directory restores.
	if err := os.Remove(bad); err != nil {
		t.Fatal(err)
	}
	rr, err := RestoreDecisions(ctx, sdb, []string{dir})
	if err != nil || rr.Restored != 2 {
		t.Fatalf("restore = %+v %v", rr, err)
	}
}

// Restoring into a database that never held the rows advances the id counter, so
// later decisions do not reuse a restored id.
func TestRestoreIntoFreshDatabaseAdvancesIDCounter(t *testing.T) {
	src, _ := retentionDB(t)
	dir := t.TempDir()
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		seedAt(t, src, 60+float64(i), 10, nil)
	}
	if _, err := PruneDecisions(ctx, src, PruneOpts{OlderThan: DefaultRetention, Now: retNow, ArchiveDir: dir}); err != nil {
		t.Fatal(err)
	}
	dst, _ := retentionDB(t)
	if rr, err := RestoreDecisions(ctx, dst, []string{dir}); err != nil || rr.Restored != 5 {
		t.Fatalf("restore = %+v %v", rr, err)
	}
	if id := seedAt(t, dst, 0, 5, nil); id <= 5 {
		t.Fatalf("new id %d reuses a restored id", id)
	}
}

// The archive is never half-written: if it cannot be put in place, nothing is deleted.
func TestPruneArchiveRenameFailureDeletesNothing(t *testing.T) {
	sdb, _ := retentionDB(t)
	dir := t.TempDir()
	ctx := context.Background()
	seedAt(t, sdb, 60, 10, nil)
	old, _ := selectOldBatch(ctx, sdb, 0, retNow.Add(-DefaultRetention).Unix(), 10)
	var rows []Decision
	for _, o := range old {
		rows = append(rows, o.Decision)
	}
	paths, err := writeArchive(dir, rows)
	if err != nil {
		t.Fatal(err)
	}
	// Block the final name with a non-empty directory so the rename cannot succeed.
	if err := os.Remove(paths[0]); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(paths[0], "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := PruneDecisions(ctx, sdb, PruneOpts{OlderThan: DefaultRetention, Now: retNow, ArchiveDir: dir}); err == nil {
		t.Fatal("expected an archive error")
	}
	if countRows(t, sdb) != 1 {
		t.Fatal("row deleted although its archive was not put in place")
	}
	if m, _ := filepath.Glob(filepath.Join(dir, ".routing-decisions-*.tmp")); len(m) != 0 {
		t.Fatalf("temp files left behind: %v", m)
	}
}

func TestPruneDecisionsArchiveFailureDeletesNothing(t *testing.T) {
	sdb, _ := retentionDB(t)
	seedAt(t, sdb, 90, 10, nil)
	seedAt(t, sdb, 1, 10, nil)
	notADir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := PruneDecisions(context.Background(), sdb, PruneOpts{OlderThan: DefaultRetention, Now: retNow, ArchiveDir: notADir}); err == nil {
		t.Fatal("expected an error when the archive directory cannot be created")
	}
	if countRows(t, sdb) != 2 {
		t.Fatal("rows were deleted although the archive could not be written")
	}
}

func TestPruneThenRestoreRoundTripsEveryColumn(t *testing.T) {
	sdb, _ := retentionDB(t)
	dir := t.TempDir()
	ctx := context.Background()
	// Spans three UTC months; small batches give several gzip members per file.
	var want []Decision
	store := NewDecisionStore(sdb)
	for i, d := range []float64{150, 149, 120, 119, 118, 100, 99, 75, 61, 45} {
		id := seedAt(t, sdb, d, 100+i, func(o *RecordDecisionOpts) {
			if i%2 == 0 {
				o.DispatchID, o.RunID, o.OverrideID = fmt.Sprintf("dx-%d", i), fmt.Sprintf("rx-%d", i), "ov"
			} else {
				o.FloorApplied, o.FloorFrom, o.FloorTo, o.Complexity = false, "", "", 0
			}
		})
		got, err := store.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, *got)
	}
	seedAt(t, sdb, 1, 10, nil)

	res, err := PruneDecisions(ctx, sdb, PruneOpts{OlderThan: DefaultRetention, Now: retNow, ArchiveDir: dir, BatchSize: 3})
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != int64(len(want)) || len(res.Files) < 3 {
		t.Fatalf("result = %+v, want %d deleted across 3+ monthly files", res, len(want))
	}
	if countRows(t, sdb) != 1 {
		t.Fatalf("rows left = %d, want 1", countRows(t, sdb))
	}

	rr, err := RestoreDecisions(ctx, sdb, res.Files)
	if err != nil {
		t.Fatal(err)
	}
	if rr.Restored != int64(len(want)) || rr.Existing != 0 {
		t.Fatalf("restore = %+v", rr)
	}
	for _, w := range want {
		got, err := store.Get(ctx, w.ID)
		if err != nil {
			t.Fatalf("restored row %d missing: %v", w.ID, err)
		}
		if !reflect.DeepEqual(*got, w) {
			t.Fatalf("row %d changed by the round trip:\n got %+v\nwant %+v", w.ID, *got, w)
		}
	}
	// Idempotent.
	rr, err = RestoreDecisions(ctx, sdb, res.Files)
	if err != nil || rr.Restored != 0 || rr.Existing != int64(len(want)) {
		t.Fatalf("second restore = %+v err=%v", rr, err)
	}
	// New rows keep taking fresh ids after a restore.
	if id := seedAt(t, sdb, 0, 5, nil); id <= want[len(want)-1].ID {
		t.Fatalf("new id %d collides with restored ids", id)
	}
}

func TestPruneDoesNotChangeSchemaVersion(t *testing.T) {
	sdb, _ := retentionDB(t)
	var before, after int
	sdb.QueryRow(`PRAGMA user_version`).Scan(&before)
	seedAt(t, sdb, 90, 10, nil)
	if _, err := PruneDecisions(context.Background(), sdb, PruneOpts{OlderThan: DefaultRetention, Now: retNow}); err != nil {
		t.Fatal(err)
	}
	sdb.QueryRow(`PRAGMA user_version`).Scan(&after)
	if before != after {
		t.Fatalf("user_version %d -> %d: a prune must not need a newer binary", before, after)
	}
}

// A database the size of the live one at the time of the sweep: 29,473 rows from
// 2026-09-22 to 2026-11-09 with about 5.2 KB of context JSON each (a synthetic
// copy, never the live file). Shows what the prune buys and that restore undoes it.
func TestPruneRealSizedDatabase(t *testing.T) {
	if testing.Short() {
		t.Skip("real-sized database: skipped with -short")
	}
	sdb, path := retentionDB(t)
	ctx := context.Background()
	const rows = 29473
	start := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC).Unix()
	end := retNow.Unix()
	tx, err := sdb.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.Prepare(`INSERT INTO routing_decisions (project_dir, agent, selected_model, rule_matched, candidates, context_json, decided_at)
		VALUES ('/home/x/proj', ?, 'sonnet', 'default', '["sonnet","opus"]', ?, ?)`)
	if err != nil {
		t.Fatal(err)
	}
	pad := strings.Repeat("0123456789abcdef", 330) // ~5.2 KB with the wrapper
	for i := 0; i < rows; i++ {
		ts := start + int64(i)*(end-start)/rows
		if _, err := stmt.Exec(fmt.Sprintf("agent-%d", i%40), fmt.Sprintf(`{"n":%d,"pad":"%s"}`, i, pad), ts); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := sdb.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	size := func() int64 { st, _ := os.Stat(path); return st.Size() }
	before := size()

	t0 := time.Now()
	res, err := PruneDecisions(ctx, sdb, PruneOpts{OlderThan: DefaultRetention, Now: retNow, ArchiveDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	took := time.Since(t0)
	left := countRows(t, sdb)
	if res.Deleted+left != rows {
		t.Fatalf("deleted %d + left %d != %d", res.Deleted, left, rows)
	}
	// Rows are spread evenly over end-start seconds; the cutoff is 30 days before end.
	wantDeleted := int64(0)
	for i := 0; i < rows; i++ {
		if start+int64(i)*(end-start)/rows < res.Cutoff {
			wantDeleted++
		}
	}
	if res.Deleted != wantDeleted || wantDeleted == 0 || left == 0 {
		t.Fatalf("deleted %d, want %d (left %d)", res.Deleted, wantDeleted, left)
	}
	var oldest int64
	sdb.QueryRow(`SELECT MIN(decided_at) FROM routing_decisions`).Scan(&oldest)
	if oldest < res.Cutoff {
		t.Fatalf("a row older than the cutoff survived (%d < %d)", oldest, res.Cutoff)
	}
	if _, err := sdb.Exec(`VACUUM`); err != nil {
		t.Fatal(err)
	}
	sdb.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	after := size()
	if after >= before*3/4 {
		t.Fatalf("file %d -> %d bytes after prune+vacuum: expected a clear reduction", before, after)
	}
	var archived int64
	for _, f := range res.Files {
		st, _ := os.Stat(f)
		archived += st.Size()
	}
	t.Logf("%d rows, %.0f MB; pruned %d in %v; left %d, %.0f MB after VACUUM; archive %.1f MB", rows, float64(before)/1e6, res.Deleted, took, left, float64(after)/1e6, float64(archived)/1e6)

	// An immediate second run has nothing to do and must be cheap (it still reads every row; the hook runs it once a day).
	t0 = time.Now()
	res2, err := PruneDecisions(ctx, sdb, PruneOpts{OlderThan: DefaultRetention, Now: retNow})
	if err != nil || res2.Eligible != 0 {
		t.Fatalf("second run: %+v %v", res2, err)
	}
	t.Logf("no-op run took %v", time.Since(t0))

	rr, err := RestoreDecisions(ctx, sdb, res.Files)
	if err != nil || rr.Restored != res.Deleted {
		t.Fatalf("restore = %+v err=%v, want %d restored", rr, err, res.Deleted)
	}
	if countRows(t, sdb) != rows {
		t.Fatalf("rows after restore = %d, want %d", countRows(t, sdb), rows)
	}
}
