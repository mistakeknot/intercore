package db

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

var schema040Tables = []string{
	"dispatch_terminals",
	"dispatch_supervision",
	"dispatch_intents",
	"dispatch_consumptions",
	"dispatch_terminal_deliveries",
}

var schema040Triggers = []string{
	"dispatches_terminal_outbox",
	"dispatches_supervision_guard",
	"dispatch_terminals_no_update",
	"dispatch_terminals_no_delete",
}

func assertSchema040Objects(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, kind := range []struct {
		typ   string
		names []string
	}{{"table", schema040Tables}, {"trigger", schema040Triggers}} {
		for _, name := range kind.names {
			var got string
			err := db.QueryRow("SELECT name FROM sqlite_master WHERE type = ? AND name = ?", kind.typ, name).Scan(&got)
			if err != nil {
				t.Errorf("%s %s missing: %v", kind.typ, name, err)
			}
		}
	}
}

func TestSchema040AddsDispatchTerminalRecords(t *testing.T) {
	d, _ := tempDB(t)
	ctx := context.Background()
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if _, err := d.db.ExecContext(ctx, "PRAGMA user_version = 39"); err != nil {
		t.Fatalf("set version: %v", err)
	}
	m, err := NewMigrator(d)
	if err != nil {
		t.Fatalf("NewMigrator: %v", err)
	}
	applied, err := m.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if want := currentSchemaVersion - 39; applied != want {
		t.Fatalf("applied = %d, want %d", applied, want)
	}
	assertSchema040Objects(t, d.db)
}

// A database at schema 39 reaches 40 through the runtime Migrate path, which is
// what `ic init` runs on existing hosts.
func TestMigrateFromV39AddsDispatchTerminalRecords(t *testing.T) {
	d, _ := tempDB(t)
	ctx := context.Background()
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	for _, trigger := range schema040Triggers {
		if _, err := d.db.ExecContext(ctx, "DROP TRIGGER IF EXISTS "+trigger); err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range schema040Tables {
		if _, err := d.db.ExecContext(ctx, "DROP TABLE IF EXISTS "+table); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.db.ExecContext(ctx, "PRAGMA user_version = 39"); err != nil {
		t.Fatal(err)
	}
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("Migrate from 39: %v", err)
	}
	if v, err := d.SchemaVersion(); err != nil || v != 40 {
		t.Fatalf("schema version = %d (%v), want 40", v, err)
	}
	assertSchema040Objects(t, d.db)
}

func schema040DB(t *testing.T) *sql.DB {
	t.Helper()
	d, _ := tempDB(t)
	if err := d.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return d.db
}

func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func expectAbort(t *testing.T, err error, fragment string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), fragment) {
		t.Fatalf("err = %v, want an abort mentioning %q", err, fragment)
	}
}

// A status write that bypasses the terminal writer still leaves a terminal
// record and a terminal event, marked unrecorded so it never counts as evidence.
func TestTriggerRowCarriesEventAndColumns(t *testing.T) {
	db := schema040DB(t)
	mustExec(t, db, "INSERT INTO dispatches (id, project_dir, status, retry_count) VALUES ('raw', '/tmp/p', 'running', 2)")
	mustExec(t, db, "UPDATE dispatches SET status = 'failed' WHERE id = 'raw'")

	var status, from, source, evidence string
	var attempt int
	var eventID int64
	err := db.QueryRow(`SELECT status, from_status, terminal_source, evidence_status, attempt, event_id
		FROM dispatch_terminals WHERE dispatch_id = 'raw'`).Scan(&status, &from, &source, &evidence, &attempt, &eventID)
	if err != nil {
		t.Fatalf("terminal record: %v", err)
	}
	if status != "failed" || from != "running" || source != "trigger" || evidence != "unrecorded" || attempt != 2 {
		t.Fatalf("terminal record = %s from %s source %s evidence %s attempt %d", status, from, source, evidence, attempt)
	}
	var eventType, toStatus string
	if err := db.QueryRow("SELECT event_type, to_status FROM dispatch_events WHERE id = ?", eventID).Scan(&eventType, &toStatus); err != nil {
		t.Fatalf("terminal event %d: %v", eventID, err)
	}
	if eventType != "terminal" || toStatus != "failed" {
		t.Fatalf("event = %s to %s, want terminal to failed", eventType, toStatus)
	}
}

// A supervised dispatch may only become terminal through a writer that records
// the terminal record first; a bare status update is refused.
func TestSupervisionGuardRefusesBareTerminalUpdate(t *testing.T) {
	db := schema040DB(t)
	mustExec(t, db, "INSERT INTO dispatches (id, project_dir, status) VALUES ('sup', '/tmp/p', 'running')")
	mustExec(t, db, "INSERT INTO dispatch_supervision (dispatch_id, db_path, prompt_sha256, created_at) VALUES ('sup', '/tmp/p/intercore.db', 'abc', 1)")
	_, err := db.Exec("UPDATE dispatches SET status = 'completed' WHERE id = 'sup'")
	expectAbort(t, err, "supervised dispatch")

	var status string
	if err := db.QueryRow("SELECT status FROM dispatches WHERE id = 'sup'").Scan(&status); err != nil || status != "running" {
		t.Fatalf("status = %q (%v), want running after refused update", status, err)
	}
}

// The terminal writer's order: event, terminal record, then the status update.
// The guard lets it through and the outbox trigger adds nothing.
func TestTerminalWriterOrderPassesGuardWithoutDuplicates(t *testing.T) {
	db := schema040DB(t)
	mustExec(t, db, "INSERT INTO dispatches (id, project_dir, status) VALUES ('w', '/tmp/p', 'running')")
	mustExec(t, db, "INSERT INTO dispatch_supervision (dispatch_id, db_path, prompt_sha256, created_at) VALUES ('w', '/tmp/p/intercore.db', 'abc', 1)")

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	res, err := tx.Exec("INSERT INTO dispatch_events (dispatch_id, from_status, to_status, event_type) VALUES ('w', 'running', 'completed', 'terminal')")
	if err != nil {
		t.Fatal(err)
	}
	eventID, _ := res.LastInsertId()
	if _, err := tx.Exec(`INSERT INTO dispatch_terminals
		(dispatch_id, attempt, status, from_status, terminal_source, evidence_status, event_id, created_at)
		VALUES ('w', 0, 'completed', 'running', 'supervisor', 'verified', ?, 1)`, eventID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("UPDATE dispatches SET status = 'completed' WHERE id = 'w' AND status NOT IN ('completed','failed','timeout','cancelled')"); err != nil {
		t.Fatalf("guarded status update after terminal record: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	var terminals, events int
	db.QueryRow("SELECT count(*) FROM dispatch_terminals WHERE dispatch_id = 'w'").Scan(&terminals)
	db.QueryRow("SELECT count(*) FROM dispatch_events WHERE dispatch_id = 'w' AND event_type = 'terminal'").Scan(&events)
	if terminals != 1 || events != 1 {
		t.Fatalf("terminal records = %d, terminal events = %d, want 1 and 1", terminals, events)
	}
}

func TestTerminalRecordsAreImmutable(t *testing.T) {
	db := schema040DB(t)
	mustExec(t, db, "INSERT INTO dispatches (id, project_dir, status) VALUES ('i', '/tmp/p', 'running')")
	mustExec(t, db, "UPDATE dispatches SET status = 'failed' WHERE id = 'i'")
	_, err := db.Exec("UPDATE dispatch_terminals SET status = 'completed' WHERE dispatch_id = 'i'")
	expectAbort(t, err, "immutable")
}

// Erratum 3.1: a terminal record can be removed only after its dispatch row,
// which is the order prune already uses.
func TestTerminalRowDeleteNeedsDispatchGone(t *testing.T) {
	db := schema040DB(t)
	mustExec(t, db, "INSERT INTO dispatches (id, project_dir, status) VALUES ('p', '/tmp/p', 'running')")
	mustExec(t, db, "UPDATE dispatches SET status = 'failed' WHERE id = 'p'")

	_, err := db.Exec("DELETE FROM dispatch_terminals WHERE dispatch_id = 'p'")
	expectAbort(t, err, "only with their dispatch")

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("DELETE FROM dispatches WHERE id = 'p'"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("DELETE FROM dispatch_terminals WHERE dispatch_id = 'p'"); err != nil {
		t.Fatalf("delete terminal record after its dispatch: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
