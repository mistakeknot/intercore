-- v25: canonical landed-change entity
-- Replaces three competing "landed change" definitions with a single
-- durable, attributable outcome record. See iv-fo0rx.
CREATE TABLE IF NOT EXISTS landed_changes (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    commit_sha      TEXT NOT NULL,
    project_dir     TEXT NOT NULL,
    branch          TEXT NOT NULL DEFAULT 'main',
    dispatch_id     TEXT,
    run_id          TEXT,
    bead_id         TEXT,
    session_id      TEXT,
    merge_intent_id INTEGER,
    landed_at       INTEGER NOT NULL DEFAULT (unixepoch()),
    reverted_at     INTEGER,
    reverted_by     TEXT,
    files_changed   INTEGER,
    insertions      INTEGER,
    deletions       INTEGER,
    UNIQUE(commit_sha, project_dir)
);
CREATE INDEX IF NOT EXISTS idx_landed_changes_project ON landed_changes(project_dir, landed_at);
CREATE INDEX IF NOT EXISTS idx_landed_changes_bead ON landed_changes(bead_id) WHERE bead_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_landed_changes_dispatch ON landed_changes(dispatch_id) WHERE dispatch_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_landed_changes_run ON landed_changes(run_id) WHERE run_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_landed_changes_session ON landed_changes(session_id) WHERE session_id IS NOT NULL;
