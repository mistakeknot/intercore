package autonomy

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func setupStateDB(t *testing.T) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	dsn := "file:" + filepath.Join(dir, "test.db") + "?_pragma=busy_timeout%3D5000"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })

	// Mirrors the shipped `state` table. ResolveDB has to work against the real
	// shape, including expires_at, because it is the reader other modules use.
	if _, err := db.Exec(`
		CREATE TABLE state (
			key TEXT NOT NULL,
			scope_id TEXT NOT NULL,
			payload TEXT NOT NULL,
			updated_at INTEGER NOT NULL DEFAULT (unixepoch()),
			expires_at INTEGER,
			PRIMARY KEY (key, scope_id)
		)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	return db
}

func setLevel(t *testing.T, db *sql.DB, payload string, expiresAt any) {
	t.Helper()
	_, err := db.Exec(
		`INSERT OR REPLACE INTO state (key, scope_id, payload, expires_at) VALUES (?, ?, ?, ?)`,
		StateKey, Scope, payload, expiresAt)
	if err != nil {
		t.Fatalf("insert state: %v", err)
	}
}

func TestMinLevelForAuto(t *testing.T) {
	for _, op := range []string{"git-push-main", "bd-push-dolt"} {
		if got := MinLevelForAuto(op); got != PushAutoFloor {
			t.Errorf("MinLevelForAuto(%q) = %d, want %d", op, got, PushAutoFloor)
		}
	}
	// An op with no floor must be governed by policy alone — the ceiling is
	// opt-in per operation, not a blanket downgrade of every gated command.
	for _, op := range []string{"bead-close", "ic-publish-patch", "unknown-op", ""} {
		if got := MinLevelForAuto(op); got != 0 {
			t.Errorf("MinLevelForAuto(%q) = %d, want 0", op, got)
		}
	}
}

func TestFlooredOpsIsACopy(t *testing.T) {
	got := FlooredOps()
	if len(got) != 2 {
		t.Fatalf("FlooredOps() has %d entries, want 2", len(got))
	}
	got["git-push-main"] = 0
	if MinLevelForAuto("git-push-main") != PushAutoFloor {
		t.Error("mutating the FlooredOps result changed the real table")
	}
}

func TestPermitsAuto(t *testing.T) {
	for level := MinLevel; level <= MaxLevel; level++ {
		r := Resolution{Level: level}
		wantPush := level >= PushAutoFloor
		if got := r.PermitsAuto("git-push-main"); got != wantPush {
			t.Errorf("L%d PermitsAuto(git-push-main) = %t, want %t", level, got, wantPush)
		}
		if !r.PermitsAuto("bead-close") {
			t.Errorf("L%d must permit an op with no delegation floor", level)
		}
	}
}

func TestDefaultLevelFailsClosedForPushes(t *testing.T) {
	// The fail-closed property is not special-case error handling: it falls out
	// of DefaultLevel sitting below PushAutoFloor. If someone raises the default
	// this test is where they find out what else it changes.
	if DefaultLevel >= PushAutoFloor {
		t.Fatalf("DefaultLevel %d >= PushAutoFloor %d: an unreadable or undeclared "+
			"level would silently authorize pushes", DefaultLevel, PushAutoFloor)
	}
}

func TestExplainRefusalNamesTheLevel(t *testing.T) {
	declared := Resolution{Level: 1, Name: Name(1), Declared: true}
	msg := declared.ExplainRefusal("git-push-main")
	for _, want := range []string{"L1", Name(1), "L3", "git-push-main"} {
		if !strings.Contains(msg, want) {
			t.Errorf("declared refusal %q missing %q", msg, want)
		}
	}
	if strings.Contains(msg, "undeclared") {
		t.Errorf("declared refusal must not claim the level is undeclared: %q", msg)
	}

	// An undeclared level behaves like L2 but nobody chose it. Saying so is the
	// difference between "you set this" and "this is what you got by default".
	fallback := Resolution{Level: DefaultLevel, Name: Name(DefaultLevel)}
	msg = fallback.ExplainRefusal("git-push-main")
	if !strings.Contains(msg, "undeclared") {
		t.Errorf("undeclared refusal %q must say so", msg)
	}
}

func TestResolveDBUnset(t *testing.T) {
	res := ResolveDB(context.Background(), setupStateDB(t))
	if res.Level != DefaultLevel || res.Declared {
		t.Errorf("unset = %+v, want the undeclared default", res)
	}
}

func TestResolveDBDeclared(t *testing.T) {
	db := setupStateDB(t)
	setLevel(t, db, "3", nil)
	res := ResolveDB(context.Background(), db)
	if res.Level != 3 || !res.Declared {
		t.Errorf("declared = %+v, want L3 declared", res)
	}
	if !res.PermitsAuto("git-push-main") {
		t.Error("L3 must clear the push floor")
	}
}

func TestResolveDBHonorsExpiry(t *testing.T) {
	// dbGetter restates the expiry clause that internal/state.Store uses. If the
	// two ever drift, an expired L3 row would keep authorizing pushes.
	db := setupStateDB(t)
	setLevel(t, db, "3", 1)
	res := ResolveDB(context.Background(), db)
	if res.Declared || res.Level != DefaultLevel {
		t.Errorf("expired row = %+v, want the undeclared default", res)
	}
}

func TestResolveDBMissingTable(t *testing.T) {
	// A kernel that cannot be read at all still has to produce a decision.
	dir := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "empty.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	res := ResolveDB(context.Background(), db)
	if res.Level != DefaultLevel || res.Declared {
		t.Errorf("missing table = %+v, want the undeclared default", res)
	}
	if res.PermitsAuto("git-push-main") {
		t.Error("an unreadable kernel must not authorize a push")
	}
}

func TestResolveDBNil(t *testing.T) {
	res := ResolveDB(context.Background(), nil)
	if res.Level != DefaultLevel || res.Declared {
		t.Errorf("nil db = %+v, want the undeclared default", res)
	}
}
