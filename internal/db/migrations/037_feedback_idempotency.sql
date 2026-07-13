CREATE TABLE IF NOT EXISTS feedback_idempotency (
    idempotency_key TEXT PRIMARY KEY,
    discovery_id    TEXT NOT NULL,
    signal_type     TEXT NOT NULL,
    signal_data     TEXT NOT NULL,
    actor           TEXT NOT NULL,
    signal_id       INTEGER,
    created_at      INTEGER NOT NULL DEFAULT (unixepoch())
);
CREATE INDEX IF NOT EXISTS idx_feedback_idempotency_discovery
    ON feedback_idempotency(discovery_id);
