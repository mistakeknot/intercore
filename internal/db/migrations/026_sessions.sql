-- v26: durable session attribution ledger (iv-30zy3)
-- Replaces temp-file attribution (/tmp/interstat-*) with kernel-backed
-- session lifecycle and attribution event tracking.

CREATE TABLE IF NOT EXISTS sessions (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id      TEXT NOT NULL,
    project_dir     TEXT NOT NULL,
    agent_type      TEXT NOT NULL DEFAULT 'claude-code',
    model           TEXT,
    started_at      INTEGER NOT NULL DEFAULT (unixepoch()),
    ended_at        INTEGER,
    metadata        TEXT,
    UNIQUE(session_id, project_dir)
);
CREATE INDEX IF NOT EXISTS idx_sessions_project ON sessions(project_dir, started_at);
CREATE INDEX IF NOT EXISTS idx_sessions_started ON sessions(started_at);

CREATE TABLE IF NOT EXISTS session_attributions (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id      TEXT NOT NULL,
    project_dir     TEXT NOT NULL,
    bead_id         TEXT,
    run_id          TEXT,
    phase           TEXT,
    created_at      INTEGER NOT NULL DEFAULT (unixepoch())
);
CREATE INDEX IF NOT EXISTS idx_session_attr_session ON session_attributions(session_id, project_dir, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_session_attr_bead ON session_attributions(bead_id) WHERE bead_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_session_attr_created ON session_attributions(created_at);
