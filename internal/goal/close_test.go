package goal

import (
	"context"
	"errors"
	"testing"
	"time"
)

func mkGoal(t *testing.T, s *Store) string {
	t.Helper()
	id, err := s.Create(context.Background(),
		&Goal{ProjectDir: "/tmp/t", Title: "g", ConditionText: "tests exit 0"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return id
}

func TestAcquireClose_ExclusiveAndFenced(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	id := mkGoal(t, s)

	f1, err := s.AcquireClose(ctx, id, "run-A", "sessionA", 3600)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if f1 != 1 {
		t.Errorf("fence = %d, want 1", f1)
	}
	if _, err := s.AcquireClose(ctx, id, "run-B", "sessionB", 3600); !errors.Is(err, ErrLeaseHeld) {
		t.Errorf("second acquire err = %v, want ErrLeaseHeld", err)
	}
}

func TestAcquireClose_BreaksExpiredLease_StaleFenceRejected(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	id := mkGoal(t, s)

	f1, err := s.AcquireClose(ctx, id, "run-A", "sessionA", 0) // expires immediately
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	time.Sleep(1100 * time.Millisecond) // ensure now > lease_expires_at (second resolution)
	f2, err := s.AcquireClose(ctx, id, "run-B", "sessionB", 3600)
	if err != nil {
		t.Fatalf("break-stale acquire: %v", err)
	}
	if f2 != f1+1 {
		t.Errorf("fence after break = %d, want %d", f2, f1+1)
	}
	// old holder's fence is now stale
	if err := s.StampStep(ctx, id, "verified", f1); !errors.Is(err, ErrStaleFence) {
		t.Errorf("stale stamp err = %v, want ErrStaleFence", err)
	}
	// new holder stamps fine
	if err := s.StampStep(ctx, id, "verified", f2); err != nil {
		t.Errorf("fresh stamp: %v", err)
	}
}

func TestFinishClose_RequiresAllSteps(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	id := mkGoal(t, s)
	f, _ := s.AcquireClose(ctx, id, "run-A", "sessionA", 3600)

	if err := s.FinishClose(ctx, id, f); !errors.Is(err, ErrCloseIncomplete) {
		t.Fatalf("premature finish err = %v, want ErrCloseIncomplete", err)
	}
	for _, step := range []string{"verified", "reflected", "compounded", "successor_proposed"} {
		if err := s.StampStep(ctx, id, step, f); err != nil {
			t.Fatalf("stamp %s: %v", step, err)
		}
	}
	if err := s.FinishClose(ctx, id, f); err != nil {
		t.Fatalf("finish: %v", err)
	}
	g, _ := s.Get(ctx, id)
	if g.Status != "closed" || g.ClosedAt == nil {
		t.Errorf("after close: %+v", g)
	}
}

func TestStampStep_UnknownStepRejected(t *testing.T) {
	s := setupTestStore(t)
	id := mkGoal(t, s)
	f, _ := s.AcquireClose(context.Background(), id, "r", "o", 3600)
	if err := s.StampStep(context.Background(), id, "bogus", f); err == nil {
		t.Error("bogus step accepted")
	}
}
