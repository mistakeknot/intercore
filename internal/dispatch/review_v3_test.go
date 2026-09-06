package dispatch

import (
	"context"
	"testing"
	"time"
)

func TestReviewV3PruneProcessIdentities(t *testing.T) {
	for _, rollback := range []bool{false, true} {
		t.Run(map[bool]string{false: "cleanup", true: "rollback"}[rollback], func(t *testing.T) {
			s := testStore(t)
			_, err := s.db.Exec(`INSERT INTO dispatches(id,project_dir,status,created_at) VALUES
				('old','.', 'completed',0), ('live','.', 'running',0), ('recent','.', 'completed',unixepoch());
				INSERT INTO state(key,scope_id,payload) VALUES
				('dispatch.process','old','{}'), ('dispatch.process','live','{}'),
				('dispatch.process','recent','{}'), ('dispatch.process','orphan','{}'), ('other','orphan','{}')`)
			if err != nil {
				t.Fatal(err)
			}
			if rollback {
				if _, err := s.db.Exec(`CREATE TRIGGER refuse_identity_prune BEFORE DELETE ON state WHEN OLD.key='dispatch.process' BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
					t.Fatal(err)
				}
			}
			n, err := s.Prune(context.Background(), time.Hour)
			if rollback {
				if err == nil {
					t.Fatal("identity cleanup failure did not abort prune")
				}
				var count int
				if err := s.db.QueryRow(`SELECT count(*) FROM dispatches WHERE id='old'`).Scan(&count); err != nil || count != 1 {
					t.Fatalf("prune lost attempt: count=%d err=%v", count, err)
				}
				return
			}
			if err != nil || n != 1 {
				t.Fatalf("prune=%d err=%v", n, err)
			}
			for scope, want := range map[string]int{"old": 0, "orphan": 0, "live": 1, "recent": 1} {
				var count int
				if err := s.db.QueryRow(`SELECT count(*) FROM state WHERE key='dispatch.process' AND scope_id=?`, scope).Scan(&count); err != nil || count != want {
					t.Fatalf("scope=%s count=%d want=%d err=%v", scope, count, want, err)
				}
			}
			var count int
			if err := s.db.QueryRow(`SELECT count(*) FROM state WHERE key='other'`).Scan(&count); err != nil || count != 1 {
				t.Fatalf("unrelated state lost: %d %v", count, err)
			}
		})
	}
}
