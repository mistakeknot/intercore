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

-- v3: phase state machine (runs + events)
CREATE TABLE IF NOT EXISTS runs (
    id              TEXT NOT NULL PRIMARY KEY,
    project_dir     TEXT NOT NULL,
    goal            TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'active',
    phase           TEXT NOT NULL DEFAULT 'brainstorm',
    complexity      INTEGER NOT NULL DEFAULT 3,
    force_full      INTEGER NOT NULL DEFAULT 0,
    auto_advance    INTEGER NOT NULL DEFAULT 1,
    created_at      INTEGER NOT NULL DEFAULT (unixepoch()),
    updated_at      INTEGER NOT NULL DEFAULT (unixepoch()),
    completed_at    INTEGER,
    scope_id        TEXT,
    metadata        TEXT
);
CREATE INDEX IF NOT EXISTS idx_runs_status ON runs(status) WHERE status = 'active';

CREATE TABLE IF NOT EXISTS phase_events (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id          TEXT NOT NULL REFERENCES runs(id),
    from_phase      TEXT NOT NULL,
    to_phase        TEXT NOT NULL,
    event_type      TEXT NOT NULL DEFAULT 'advance',
    gate_result     TEXT,
    gate_tier       TEXT,
    reason          TEXT,
    created_at      INTEGER NOT NULL DEFAULT (unixepoch())
);
CREATE INDEX IF NOT EXISTS idx_phase_events_run ON phase_events(run_id);

-- v4: run tracking (agents + artifacts)
CREATE TABLE IF NOT EXISTS run_agents (
    id          TEXT NOT NULL PRIMARY KEY,
    run_id      TEXT NOT NULL REFERENCES runs(id),
    agent_type  TEXT NOT NULL DEFAULT 'claude',
    name        TEXT,
    status      TEXT NOT NULL DEFAULT 'active',
    dispatch_id TEXT,
    created_at  INTEGER NOT NULL DEFAULT (unixepoch()),
    updated_at  INTEGER NOT NULL DEFAULT (unixepoch())
);
CREATE INDEX IF NOT EXISTS idx_run_agents_run ON run_agents(run_id);
CREATE INDEX IF NOT EXISTS idx_run_agents_status ON run_agents(status) WHERE status = 'active';

CREATE TABLE IF NOT EXISTS run_artifacts (
    id          TEXT NOT NULL PRIMARY KEY,
    run_id      TEXT NOT NULL REFERENCES runs(id),
    phase       TEXT NOT NULL,
    path        TEXT NOT NULL,
    type        TEXT NOT NULL DEFAULT 'file',
    created_at  INTEGER NOT NULL DEFAULT (unixepoch())
);
CREATE INDEX IF NOT EXISTS idx_run_artifacts_run ON run_artifacts(run_id);
CREATE INDEX IF NOT EXISTS idx_run_artifacts_phase ON run_artifacts(run_id, phase);
