-- v24: review events (disagreement resolution pipeline)
CREATE TABLE IF NOT EXISTS review_events (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id            TEXT,
    finding_id        TEXT NOT NULL,
    agents_json       TEXT NOT NULL,
    resolution        TEXT NOT NULL,
    dismissal_reason  TEXT,
    chosen_severity   TEXT NOT NULL,
    impact            TEXT NOT NULL,
    session_id        TEXT,
    project_dir       TEXT,
    created_at        INTEGER NOT NULL DEFAULT (unixepoch())
);
CREATE INDEX IF NOT EXISTS idx_review_events_finding ON review_events(finding_id);
CREATE INDEX IF NOT EXISTS idx_review_events_created ON review_events(created_at);
