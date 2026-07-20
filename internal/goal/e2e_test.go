package goal_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/mistakeknot/intercore/internal/db"
	"github.com/mistakeknot/intercore/internal/goal"
	"github.com/mistakeknot/intercore/internal/phase"
)

// TestGoalLifecycleE2E: mint → lint → run attach → advance (dormancy touch)
// → audit clean → fenced close sequence → successor → audit clean.
func TestGoalLifecycleE2E(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(filepath.Join(t.TempDir(), "e2e.db"), 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	gs := goal.New(d.SqlDB())
	ps := phase.New(d.SqlDB())

	cond := "`go test ./...` exits 0, or stop after 20 turns"
	if probs := goal.LintCondition(cond); len(probs) != 0 {
		t.Fatalf("condition should lint clean: %v", probs)
	}
	gid, err := gs.Create(ctx, &goal.Goal{ProjectDir: "/tmp/p", Title: "e2e", ConditionText: cond})
	if err != nil {
		t.Fatal(err)
	}

	rid, err := ps.Create(ctx, &phase.Run{ProjectDir: "/tmp/p", Goal: "e2e", Complexity: 2,
		AutoAdvance: true, GoalID: &gid})
	if err != nil {
		t.Fatal(err)
	}
	if err := ps.UpdatePhase(ctx, rid, "brainstorm", "brainstorm-reviewed"); err != nil {
		t.Fatal(err)
	}

	if defects, _ := gs.Audit(ctx, "/tmp/p", 3600); len(defects) != 0 {
		t.Fatalf("healthy goal audited dirty: %v", defects)
	}

	fence, err := gs.AcquireClose(ctx, gid, rid, "e2e-session", 3600)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []string{"verified", "reflected", "compounded", "successor_proposed"} {
		if err := gs.StampStep(ctx, gid, step, fence); err != nil {
			t.Fatalf("stamp %s: %v", step, err)
		}
	}
	if err := gs.SetSuccessor(ctx, gid, "bead:next-1"); err != nil {
		t.Fatal(err)
	}
	if err := gs.FinishClose(ctx, gid, fence); err != nil {
		t.Fatal(err)
	}
	if defects, _ := gs.Audit(ctx, "/tmp/p", 3600); len(defects) != 0 {
		t.Fatalf("closed-with-successor audited dirty: %v", defects)
	}
}
