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
		Now:                now,
		VettedAt:           now.Add(-64 * time.Minute),
		ClockSkewTolerance: 5 * time.Minute,
	})
	if !ok {
		t.Fatal("expected 64m age with 60m rule and 5m tolerance to pass")
	}

	ok, _ = Evaluate(rule, CheckInput{
		Now:                now,
		VettedAt:           now.Add(-66 * time.Minute),
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

// committed_by_this_session was a stub that always failed when required; it now
// reads CheckInput.CommittedByThisSession like the other boolean requirements.
func TestEvaluate_CommittedByThisSession(t *testing.T) {
	rule := Rule{
		Op:   "git-push-main",
		Mode: ModeAuto,
		Requires: map[string]interface{}{
			"committed_by_this_session": true,
		},
	}

	// Satisfied: HEAD was committed this session.
	if ok, reason := Evaluate(rule, CheckInput{CommittedByThisSession: true}); !ok {
		t.Errorf("require true + input true: Evaluate returned false: %s", reason)
	}

	// Unsatisfied: pre-existing unpushed work, not committed this session.
	if ok, _ := Evaluate(rule, CheckInput{CommittedByThisSession: false}); ok {
		t.Error("require true + input false: expected failure, got pass")
	}

	// require:false must pass only when the input is also false.
	ruleFalse := Rule{
		Op:       "git-push-main",
		Mode:     ModeAuto,
		Requires: map[string]interface{}{"committed_by_this_session": false},
	}
	if ok, _ := Evaluate(ruleFalse, CheckInput{CommittedByThisSession: true}); ok {
		t.Error("require false + input true: expected failure, got pass")
	}
	if ok, reason := Evaluate(ruleFalse, CheckInput{CommittedByThisSession: false}); !ok {
		t.Errorf("require false + input false: Evaluate returned false: %s", reason)
	}
}
