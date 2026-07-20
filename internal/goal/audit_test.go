package goal

import (
	"context"
	"testing"
)

func TestAudit_ThreeDefectClasses(t *testing.T) {
	const testNow int64 = 10_000
	oldNowUnix := nowUnix
	nowUnix = func() int64 { return testNow }
	t.Cleanup(func() { nowUnix = oldNowUnix })

	s := setupTestStore(t)
	ctx := context.Background()

	// dormant: open, with no attached-run advance since creation.
	dormantID := mkGoal(t, s)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE goals SET created_at = ?, last_run_advanced_at = NULL WHERE id = ?`,
		testNow-7_200, dormantID); err != nil {
		t.Fatal(err)
	}

	// stuck_closing: lease expired mid-close, but run activity is recent.
	stuckID := mkGoal(t, s)
	if err := s.TouchRunAdvance(ctx, stuckID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireClose(ctx, stuckID, "r", "o", 3_600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE goals SET lease_expires_at = ? WHERE id = ?`, testNow-1, stuckID); err != nil {
		t.Fatal(err)
	}

	// closed_without_successor: closed but successor_ref NULL (legacy/manual row).
	orphanID := mkGoal(t, s)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE goals SET status = 'closed', closed_at = ? WHERE id = ?`,
		testNow-1, orphanID); err != nil {
		t.Fatal(err)
	}

	// A closing goal remains subject to the dormancy sweep even with a live lease.
	closingDormantID := mkGoal(t, s)
	if _, err := s.AcquireClose(ctx, closingDormantID, "r2", "o2", 3_600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE goals SET last_run_advanced_at = ? WHERE id = ?`,
		testNow-7_200, closingDormantID); err != nil {
		t.Fatal(err)
	}

	// healthy: open and recently advanced.
	healthyID := mkGoal(t, s)
	if err := s.TouchRunAdvance(ctx, healthyID); err != nil {
		t.Fatal(err)
	}

	// A closed goal with a durable successor reference is healthy.
	closedWithSuccessorID := mkGoal(t, s)
	if err := s.SetSuccessor(ctx, closedWithSuccessorID, "goal-next"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE goals SET status = 'closed', closed_at = ? WHERE id = ?`,
		testNow-1, closedWithSuccessorID); err != nil {
		t.Fatal(err)
	}

	defects, err := s.Audit(ctx, "", 3_600)
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	byID := defectKindsByGoal(defects)
	assertDefectKind(t, byID, dormantID, "dormant")
	assertDefectKind(t, byID, stuckID, "stuck_closing")
	assertDefectKind(t, byID, orphanID, "closed_without_successor")
	assertDefectKind(t, byID, closingDormantID, "dormant")
	if _, ok := byID[healthyID]; ok {
		t.Errorf("healthy goal flagged: %v", defects)
	}
	if _, ok := byID[closedWithSuccessorID]; ok {
		t.Errorf("closed goal with successor flagged: %v", defects)
	}
}

func TestAudit_ClosingGoalCanHaveTwoDefects(t *testing.T) {
	const testNow int64 = 10_000
	oldNowUnix := nowUnix
	nowUnix = func() int64 { return testNow }
	t.Cleanup(func() { nowUnix = oldNowUnix })

	s := setupTestStore(t)
	ctx := context.Background()
	id := mkGoal(t, s)
	if _, err := s.AcquireClose(ctx, id, "r", "o", 3_600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE goals
		SET lease_expires_at = ?, last_run_advanced_at = ? WHERE id = ?`,
		testNow-1, testNow-7_200, id); err != nil {
		t.Fatal(err)
	}

	defects, err := s.Audit(ctx, "", 3_600)
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	byID := defectKindsByGoal(defects)
	assertDefectKind(t, byID, id, "stuck_closing")
	assertDefectKind(t, byID, id, "dormant")
}

func TestAudit_ProjectFilter(t *testing.T) {
	const testNow int64 = 10_000
	oldNowUnix := nowUnix
	nowUnix = func() int64 { return testNow }
	t.Cleanup(func() { nowUnix = oldNowUnix })

	s := setupTestStore(t)
	ctx := context.Background()
	wantedID, err := s.Create(ctx, &Goal{
		ProjectDir: "/wanted", Title: "wanted", ConditionText: "tests exit 0",
	})
	if err != nil {
		t.Fatal(err)
	}
	otherID, err := s.Create(ctx, &Goal{
		ProjectDir: "/other", Title: "other", ConditionText: "tests exit 0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE goals SET created_at = ? WHERE id IN (?, ?)`,
		testNow-7_200, wantedID, otherID); err != nil {
		t.Fatal(err)
	}

	defects, err := s.Audit(ctx, "/wanted", 3_600)
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	byID := defectKindsByGoal(defects)
	assertDefectKind(t, byID, wantedID, "dormant")
	if _, ok := byID[otherID]; ok {
		t.Errorf("goal outside project filter flagged: %v", defects)
	}
}

func defectKindsByGoal(defects []Defect) map[string]map[string]bool {
	byID := make(map[string]map[string]bool)
	for _, defect := range defects {
		if byID[defect.GoalID] == nil {
			byID[defect.GoalID] = make(map[string]bool)
		}
		byID[defect.GoalID][defect.Kind] = true
	}
	return byID
}

func assertDefectKind(t *testing.T, byID map[string]map[string]bool, id, kind string) {
	t.Helper()
	if !byID[id][kind] {
		t.Errorf("goal %s defects = %v, want %q", id, byID[id], kind)
	}
}
