CREATE TABLE IF NOT EXISTS agency_events (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id          TEXT,
    agency_name     TEXT NOT NULL,
    event_type      TEXT NOT NULL,
    cycle_id        TEXT NOT NULL,
    stage           TEXT NOT NULL,
    context_json    TEXT NOT NULL DEFAULT '{}',
    idempotency_key TEXT NOT NULL,
    project_dir     TEXT,
    created_at      INTEGER NOT NULL DEFAULT (unixepoch()),
    UNIQUE (agency_name, idempotency_key)
);
CREATE INDEX IF NOT EXISTS idx_agency_events_agency
    ON agency_events(agency_name, id);
CREATE INDEX IF NOT EXISTS idx_agency_events_run
    ON agency_events(run_id, id) WHERE run_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_agency_events_created
    ON agency_events(created_at, id);
