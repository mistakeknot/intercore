-- Migration 022: replay input capture for deterministic run replay
CREATE TABLE IF NOT EXISTS run_replay_inputs (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id       TEXT NOT NULL REFERENCES runs(id),
    kind         TEXT NOT NULL,
    input_key    TEXT,
    payload      TEXT NOT NULL DEFAULT '{}',
    artifact_ref TEXT,
    event_source TEXT,
    event_id     INTEGER,
    created_at   INTEGER NOT NULL DEFAULT (unixepoch())
);
CREATE INDEX IF NOT EXISTS idx_replay_inputs_run_created
    ON run_replay_inputs(run_id, created_at, id);
CREATE INDEX IF NOT EXISTS idx_replay_inputs_run_kind
    ON run_replay_inputs(run_id, kind, created_at, id);
