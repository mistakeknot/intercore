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

func TestRulingsIsACopy(t *testing.T) {
	got := Rulings()
	got[0].Floor = 99
	got[0].Op = "clobbered"
	for _, r := range Rulings() {
		if r.Op == "clobbered" || r.Floor == 99 {
			t.Fatal("mutating the Rulings result changed the real table")
		}
	}
}

// TestOpRulingsAreSorted keeps the generated canon table's diff stable. An
// unsorted table would reorder the rendered rows on unrelated edits and make
// `gen-autonomy-position.py --check` fail for reasons nobody changed.
func TestOpRulingsAreSorted(t *testing.T) {
	rulings := Rulings()
	for i := 1; i < len(rulings); i++ {
		if rulings[i-1].Op >= rulings[i].Op {
			t.Errorf("opRulings not sorted/unique: %q then %q",
				rulings[i-1].Op, rulings[i].Op)
		}
	}
}

// TestRulingsAgreeWithMinLevelForAuto pins the two surfaces together. The whole
// point of publishing the table is that it predicts refusals, so a table that
// disagreed with the function actually doing the refusing would be worse than
// no table at all.
func TestRulingsAgreeWithMinLevelForAuto(t *testing.T) {
	for _, r := range Rulings() {
		if got := MinLevelForAuto(r.Op); got != r.Floor {
			t.Errorf("Rulings() says %s floor=%d, MinLevelForAuto says %d",
				r.Op, r.Floor, got)
		}
	}
}

// TestEveryRulingHasAReason guards the rendered canon table: the reason column
// is generated from these strings, and a blank one ships an empty cell that
// reads as "no reason was needed" rather than "nobody wrote one".
func TestEveryRulingHasAReason(t *testing.T) {
	for _, r := range Rulings() {
		if strings.TrimSpace(r.Reason) == "" {
			t.Errorf("ruling for %q has no reason", r.Op)
		}
		if r.Floor < 0 || r.Floor > MaxLevel {
			t.Errorf("ruling for %q has floor %d, outside L%d-L%d",
				r.Op, r.Floor, MinLevel, MaxLevel)
		}
	}
}

// TestAuditedOpsAreRuledOn records the outcome of the coverage audit. These four
// are the outward-facing operations the policy names; each was considered, and
// the exempt ones are exempt on the record rather than by omission. Adding a
// fifth gated op to policy without ruling on it here is exactly the gap this
// test exists to make visible.
func TestAuditedOpsAreRuledOn(t *testing.T) {
	want := map[string]int{
		"git-push-main":    PushAutoFloor,
		"bd-push-dolt":     PushAutoFloor,
		"ic-publish-patch": 0,
		"bead-close":       0,
	}
	got := make(map[string]int, len(want))
	for _, r := range Rulings() {
		got[r.Op] = r.Floor
	}
	for op, floor := range want {
		actual, ok := got[op]
		if !ok {
			t.Errorf("op %q has no recorded ruling", op)
			continue
		}
		if actual != floor {
			t.Errorf("op %q floor = %d, want %d", op, actual, floor)
		}
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
