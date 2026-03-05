package event

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestAddIntentEvent(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	// Create the events table (standard schema)
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		run_id TEXT,
		source TEXT NOT NULL,
		type TEXT NOT NULL,
		from_state TEXT,
		to_state TEXT,
		reason TEXT,
		envelope TEXT,
		created_at INTEGER NOT NULL DEFAULT (unixepoch())
	)`)
	if err != nil {
		t.Fatal(err)
	}

	// Create the intent_events table
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS intent_events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		intent_type TEXT NOT NULL,
		bead_id TEXT NOT NULL,
		idempotency_key TEXT NOT NULL,
		session_id TEXT NOT NULL,
		run_id TEXT,
		success INTEGER NOT NULL DEFAULT 0,
		error_detail TEXT,
		created_at INTEGER NOT NULL DEFAULT (unixepoch())
	)`)
	if err != nil {
		t.Fatal(err)
	}

	store := NewStore(db)
	err = store.AddIntentEvent(context.Background(),
		"sprint.advance",
		"iv-abc123",
		"sess-x-step-5",
		"sess-123",
		"",
		true,
		"",
	)
	if err != nil {
		t.Fatalf("AddIntentEvent: %v", err)
	}

	// Verify the event was stored
	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM intent_events WHERE bead_id = 'iv-abc123'").Scan(&count)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("expected 1 intent event, got %d", count)
	}

	// Test failure case
	err = store.AddIntentEvent(context.Background(),
		"gate.enforce",
		"iv-xyz789",
		"sess-y-gate",
		"sess-456",
		"",
		false,
		"GATE_BLOCKED: plan must be reviewed first",
	)
	if err != nil {
		t.Fatalf("AddIntentEvent (failure): %v", err)
	}
}
