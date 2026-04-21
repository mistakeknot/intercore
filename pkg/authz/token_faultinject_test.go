//go:build testfault

// This file compiles only under `go test -tags=testfault`. Default builds and
// default test runs never see it, so the production ConsumeToken path keeps
// consumeFaultHook nil. See docs/canon/authz-token-model.md §5.3 for the
// partial-failure invariant this test verifies.
package authz

import (
	"errors"
	"os"
	"testing"
	"time"
)

func init() {
	consumeFaultHook = func() error {
		if os.Getenv("CONSUME_FAULT_INJECT_AFTER_UPDATE") == "1" {
			return errors.New("fault injection: after UPDATE, before INSERT")
		}
		return nil
	}
}

// TestConsumeToken_PartialFailure_Atomic forces the audit-row INSERT to fail
// between the consumed_at UPDATE and the COMMIT. The deferred Rollback must
// undo the UPDATE — the token must remain consumable on a fresh attempt.
//
// Run: CONSUME_FAULT_INJECT_AFTER_UPDATE=1 go test -tags=testfault \
//      -run TestConsumeToken_PartialFailure_Atomic ./pkg/authz/
func TestConsumeToken_PartialFailure_Atomic(t *testing.T) {
	if err := os.Setenv("CONSUME_FAULT_INJECT_AFTER_UPDATE", "1"); err != nil {
		t.Fatalf("setenv: %v", err)
	}
	defer os.Unsetenv("CONSUME_FAULT_INJECT_AFTER_UPDATE")

	db, kp := setupAuthzTestDB(t)
	now := int64(1_700_000_000)
	tok, opaque, _ := issueRoot(t, db, kp, "claude-opus-4-7", now)

	// First consume — fault hook fires, tx rolls back.
	_, err := ConsumeToken(db, kp.Pub, opaque, "claude-opus-4-7", "bead-close", "sylveste-x", now+1)
	if err == nil {
		t.Fatal("expected fault-injected consume to fail")
	}
	// Token must still be consumable.
	reloaded, err := GetToken(db, tok.ID)
	if err != nil {
		t.Fatalf("GetToken: %v", err)
	}
	if reloaded.ConsumedAt != 0 {
		t.Errorf("consumed_at set despite rollback: %d", reloaded.ConsumedAt)
	}

	// No audit row should have been written.
	var auditCount int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM authorizations WHERE op_type = ? AND agent_id = ? AND sig_version = 1`,
		"bead-close", "claude-opus-4-7",
	).Scan(&auditCount); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if auditCount != 0 {
		t.Errorf("orphan audit row present after rollback: %d rows", auditCount)
	}

	// Now disable the fault and retry — the token should consume successfully.
	os.Unsetenv("CONSUME_FAULT_INJECT_AFTER_UPDATE")
	// Use a fresh "now" so we're unambiguously past the first attempt.
	consumedTok, err := ConsumeToken(db, kp.Pub, opaque, "claude-opus-4-7", "bead-close", "sylveste-x", now+2)
	if err != nil {
		t.Fatalf("retry consume after rollback: %v", err)
	}
	if consumedTok.ConsumedAt != now+2 {
		t.Errorf("consumed_at = %d, want %d", consumedTok.ConsumedAt, now+2)
	}

	_ = time.Second // keep time import in case future assertions need it
}
