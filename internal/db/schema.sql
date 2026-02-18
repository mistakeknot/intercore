-- intercore schema v1
-- Schema version tracked via PRAGMA user_version (no separate table)

CREATE TABLE IF NOT EXISTS state (
    key         TEXT NOT NULL,
    scope_id    TEXT NOT NULL,
    payload     TEXT NOT NULL,
    updated_at  INTEGER NOT NULL DEFAULT (unixepoch()),
    expires_at  INTEGER,
    PRIMARY KEY (key, scope_id)
);

CREATE INDEX IF NOT EXISTS idx_state_scope ON state(scope_id, key);
CREATE INDEX IF NOT EXISTS idx_state_expires ON state(expires_at) WHERE expires_at IS NOT NULL;

CREATE TABLE IF NOT EXISTS sentinels (
    name        TEXT NOT NULL,
    scope_id    TEXT NOT NULL,
    last_fired  INTEGER NOT NULL DEFAULT (unixepoch()),
    PRIMARY KEY (name, scope_id)
);

-- v2: dispatch tracking
CREATE TABLE IF NOT EXISTS dispatches (
    id              TEXT NOT NULL PRIMARY KEY,
    agent_type      TEXT NOT NULL DEFAULT 'codex',
    status          TEXT NOT NULL DEFAULT 'spawned',
    project_dir     TEXT NOT NULL,
    prompt_file     TEXT,
    prompt_hash     TEXT,
    output_file     TEXT,
    verdict_file    TEXT,
    pid             INTEGER,
    exit_code       INTEGER,
    name            TEXT,
    model           TEXT,
    sandbox         TEXT DEFAULT 'workspace-write',
    timeout_sec     INTEGER,
    turns           INTEGER DEFAULT 0,
    commands        INTEGER DEFAULT 0,
    messages        INTEGER DEFAULT 0,
    input_tokens    INTEGER DEFAULT 0,
    output_tokens   INTEGER DEFAULT 0,
    created_at      INTEGER NOT NULL DEFAULT (unixepoch()),
    started_at      INTEGER,
    completed_at    INTEGER,
    verdict_status  TEXT,
    verdict_summary TEXT,
    error_message   TEXT,
    scope_id        TEXT,
    parent_id       TEXT
);
CREATE INDEX IF NOT EXISTS idx_dispatches_status ON dispatches(status) WHERE status IN ('spawned', 'running');
CREATE INDEX IF NOT EXISTS idx_dispatches_scope ON dispatches(scope_id) WHERE scope_id IS NOT NULL;
