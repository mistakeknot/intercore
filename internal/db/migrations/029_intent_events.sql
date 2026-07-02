-- v29: add intent_events table for intent submission audit trail
CREATE TABLE IF NOT EXISTS intent_events (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    intent_type     TEXT NOT NULL,
    bead_id         TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    session_id      TEXT NOT NULL,
    run_id          TEXT,
    success         INTEGER NOT NULL DEFAULT 0,
    error_detail    TEXT,
    created_at      INTEGER NOT NULL DEFAULT (unixepoch())
);
CREATE INDEX IF NOT EXISTS idx_intent_events_bead ON intent_events(bead_id);
CREATE INDEX IF NOT EXISTS idx_intent_events_created ON intent_events(created_at);
