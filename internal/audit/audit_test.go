package audit

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/mistakeknot/intercore/pkg/redaction"
	_ "modernc.org/sqlite"
)

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()

	tmpFile, err := os.CreateTemp("", "audit-test-*.db")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	tmpFile.Close()
	t.Cleanup(func() { os.Remove(tmpFile.Name()) })

	db, err := sql.Open("sqlite", tmpFile.Name())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	db.SetMaxOpenConns(1)

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS audit_log (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id   TEXT NOT NULL,
			event_type   TEXT NOT NULL,
			actor        TEXT NOT NULL,
			target       TEXT NOT NULL DEFAULT '',
			payload      TEXT NOT NULL DEFAULT '{}',
			metadata     TEXT NOT NULL DEFAULT '{}',
			prev_hash    TEXT NOT NULL DEFAULT '',
			checksum     TEXT NOT NULL,
			sequence_num INTEGER NOT NULL,
			trace_id     TEXT NOT NULL DEFAULT '',
			created_at   INTEGER NOT NULL DEFAULT (unixepoch())
		);
		CREATE INDEX IF NOT EXISTS idx_audit_log_session ON audit_log(session_id, sequence_num);
	`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}

	return db
}

func TestLogAndVerify(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	logger, err := New(db, "test-session")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Log a few entries.
	err = logger.Log(ctx, EventCommand, ActorUser, "spawn",
		map[string]interface{}{"agent": "claude-1"},
		nil,
	)
	if err != nil {
		t.Fatalf("Log 1: %v", err)
	}

	err = logger.Log(ctx, EventReserve, ActorAgent, "main.go",
		map[string]interface{}{"file": "main.go"},
		map[string]interface{}{"run_id": "run-123"},
	)
	if err != nil {
		t.Fatalf("Log 2: %v", err)
	}

	err = logger.Log(ctx, EventRelease, ActorAgent, "main.go",
		map[string]interface{}{"file": "main.go"},
		nil,
	)
	if err != nil {
		t.Fatalf("Log 3: %v", err)
	}

	// Verify integrity.
	if err := VerifyIntegrity(ctx, db, "test-session"); err != nil {
		t.Fatalf("VerifyIntegrity: %v", err)
	}
}

func TestVerifyIntegrity_DetectsTampering(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	logger, err := New(db, "tamper-test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	logger.Log(ctx, EventCommand, ActorUser, "test", nil, nil)
	logger.Log(ctx, EventCommand, ActorUser, "test2", nil, nil)

	// Tamper with the payload of the first entry.
	_, err = db.Exec(`UPDATE audit_log SET payload = '{"tampered":true}' WHERE sequence_num = 1 AND session_id = 'tamper-test'`)
	if err != nil {
		t.Fatalf("tamper: %v", err)
	}

	err = VerifyIntegrity(ctx, db, "tamper-test")
	if err == nil {
		t.Fatal("expected VerifyIntegrity to detect tampering")
	}
}

func TestVerifyIntegrity_DetectsGap(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	logger, err := New(db, "gap-test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	logger.Log(ctx, EventCommand, ActorUser, "a", nil, nil)
	logger.Log(ctx, EventCommand, ActorUser, "b", nil, nil)
	logger.Log(ctx, EventCommand, ActorUser, "c", nil, nil)

	// Delete the middle entry.
	_, err = db.Exec(`DELETE FROM audit_log WHERE sequence_num = 2 AND session_id = 'gap-test'`)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}

	err = VerifyIntegrity(ctx, db, "gap-test")
	if err == nil {
		t.Fatal("expected VerifyIntegrity to detect sequence gap")
	}
}

func TestLogWithRedaction(t *testing.T) {
	redaction.ResetPatterns()
	db := setupTestDB(t)
	ctx := context.Background()

	logger, err := New(db, "redact-test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Log an entry with a secret in the payload.
	secret := "sk-ant-" + repeatChar('x', 43)
	err = logger.Log(ctx, EventCommand, ActorUser, "test",
		map[string]interface{}{"api_key": secret},
		nil,
	)
	if err != nil {
		t.Fatalf("Log: %v", err)
	}

	// Query and verify the secret is redacted.
	entries, err := Query(ctx, db, Filter{SessionID: "redact-test"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	val, ok := entries[0].Payload["api_key"].(string)
	if !ok {
		t.Fatal("payload api_key should be string")
	}
	if val == secret {
		t.Error("secret should be redacted in persisted payload")
	}
	if len(val) == 0 {
		t.Error("redacted value should not be empty")
	}
}

func TestLogWithRedactionOff(t *testing.T) {
	redaction.ResetPatterns()
	db := setupTestDB(t)
	ctx := context.Background()

	logger, err := New(db, "noredact-test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	logger.SetRedactionConfig(redaction.Config{Mode: redaction.ModeOff})

	secret := "sk-ant-" + repeatChar('y', 43)
	err = logger.Log(ctx, EventCommand, ActorUser, "test",
		map[string]interface{}{"key": secret},
		nil,
	)
	if err != nil {
		t.Fatalf("Log: %v", err)
	}

	entries, err := Query(ctx, db, Filter{SessionID: "noredact-test"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}

	val := entries[0].Payload["key"].(string)
	if val != secret {
		t.Error("with redaction off, secret should be preserved")
	}
}

func TestQuery_Filters(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	logger, err := New(db, "query-test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	logger.Log(ctx, EventCommand, ActorUser, "target1", nil, nil)
	logger.Log(ctx, EventReserve, ActorAgent, "target2", nil, nil)
	logger.Log(ctx, EventRelease, ActorAgent, "target3", nil, nil)

	// Filter by event type.
	entries, err := Query(ctx, db, Filter{SessionID: "query-test", EventType: EventReserve})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("expected 1 reserve entry, got %d", len(entries))
	}

	// Filter by actor.
	entries, err = Query(ctx, db, Filter{SessionID: "query-test", Actor: ActorAgent})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("expected 2 agent entries, got %d", len(entries))
	}

	// Limit.
	entries, err = Query(ctx, db, Filter{SessionID: "query-test", Limit: 1})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("expected 1 entry with limit, got %d", len(entries))
	}
}

func TestQuery_TimeFilter(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	logger, err := New(db, "time-test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	logger.Log(ctx, EventCommand, ActorUser, "a", nil, nil)

	entries, err := Query(ctx, db, Filter{
		SessionID: "time-test",
		Since:     time.Now().Add(-1 * time.Hour),
		Until:     time.Now().Add(1 * time.Hour),
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("expected 1 entry in time range, got %d", len(entries))
	}

	// Future filter should return nothing.
	entries, err = Query(ctx, db, Filter{
		SessionID: "time-test",
		Since:     time.Now().Add(1 * time.Hour),
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries for future filter, got %d", len(entries))
	}
}

func TestNewLogger_ResumesChain(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	// Create logger, write entries, then create a new logger for the same session.
	logger1, _ := New(db, "resume-test")
	logger1.Log(ctx, EventCommand, ActorUser, "a", nil, nil)
	logger1.Log(ctx, EventCommand, ActorUser, "b", nil, nil)

	// New logger should resume from where the first left off.
	logger2, _ := New(db, "resume-test")
	logger2.Log(ctx, EventCommand, ActorUser, "c", nil, nil)

	// Chain should still be valid.
	if err := VerifyIntegrity(ctx, db, "resume-test"); err != nil {
		t.Fatalf("VerifyIntegrity after resume: %v", err)
	}

	entries, _ := Query(ctx, db, Filter{SessionID: "resume-test"})
	if len(entries) != 3 {
		t.Errorf("expected 3 entries, got %d", len(entries))
	}
	if entries[2].SequenceNum != 3 {
		t.Errorf("expected sequence 3, got %d", entries[2].SequenceNum)
	}
}

func TestEmptySession_Verify(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	// Verifying an empty session should succeed.
	if err := VerifyIntegrity(ctx, db, "nonexistent"); err != nil {
		t.Fatalf("VerifyIntegrity on empty session should succeed: %v", err)
	}
}

func repeatChar(c byte, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = c
	}
	return string(b)
}
