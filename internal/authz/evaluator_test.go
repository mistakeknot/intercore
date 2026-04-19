package authz

import (
	"testing"
	"time"
)

func TestEvaluate_VettedWithinMinutes_Fresh(t *testing.T) {
	now := time.Unix(2_000_000, 0)
	rule := Rule{
		Op:   "bead-close",
		Mode: ModeAuto,
		Requires: map[string]interface{}{
			"vetted_within_minutes": 60,
		},
	}
	ok, reason := Evaluate(rule, CheckInput{
		Now:      now,
		VettedAt: now.Add(-10 * time.Minute),
	})
	if !ok {
		t.Fatalf("Evaluate returned false: %s", reason)
	}
}

func TestEvaluate_VettedShaMismatch(t *testing.T) {
	rule := Rule{
		Op:   "bead-close",
		Mode: ModeAuto,
		Requires: map[string]interface{}{
			"vetted_sha_matches_head": true,
		},
	}
	ok, _ := Evaluate(rule, CheckInput{
		VettedSHA: "abc",
		HeadSHA:   "def",
	})
	if ok {
		t.Fatal("expected vetted_sha mismatch to fail")
	}
}

func TestEvaluate_ClockSkewTolerance(t *testing.T) {
	now := time.Unix(3_000_000, 0)
	rule := Rule{
		Op:   "bead-close",
		Mode: ModeAuto,
		Requires: map[string]interface{}{
			"vetted_within_minutes": 60,
		},
	}

	ok, _ := Evaluate(rule, CheckInput{
		Now:               now,
		VettedAt:          now.Add(-64 * time.Minute),
		ClockSkewTolerance: 5 * time.Minute,
	})
	if !ok {
		t.Fatal("expected 64m age with 60m rule and 5m tolerance to pass")
	}

	ok, _ = Evaluate(rule, CheckInput{
		Now:               now,
		VettedAt:          now.Add(-66 * time.Minute),
		ClockSkewTolerance: 5 * time.Minute,
	})
	if ok {
		t.Fatal("expected 66m age with 60m rule and 5m tolerance to fail")
	}
}

func TestEvaluate_MultiRepoShasAllMatch(t *testing.T) {
	rule := Rule{
		Op:   "git-push-main",
		Mode: ModeAuto,
		Requires: map[string]interface{}{
			"vetted_sha_matches_head": true,
		},
	}
	ok, reason := Evaluate(rule, CheckInput{
		VettingSHAs: map[string]string{
			"repo-a": "sha1",
			"repo-b": "sha2",
		},
		WorkdirHEAD: map[string]string{
			"repo-a": "sha1",
			"repo-b": "sha2",
		},
	})
	if !ok {
		t.Fatalf("Evaluate returned false: %s", reason)
	}
}
