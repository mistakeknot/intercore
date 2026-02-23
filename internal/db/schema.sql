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
    cache_hits      INTEGER,
    created_at      INTEGER NOT NULL DEFAULT (unixepoch()),
    started_at      INTEGER,
    completed_at    INTEGER,
    verdict_status  TEXT,
    verdict_summary TEXT,
    error_message   TEXT,
    scope_id        TEXT,
    parent_id       TEXT,
    base_repo_commit TEXT,
    retry_count      INTEGER NOT NULL DEFAULT 0,
    conflict_type    TEXT,
    quarantine_reason TEXT,
    spawn_depth       INTEGER NOT NULL DEFAULT 0,
    parent_dispatch_id TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_dispatches_status ON dispatches(status) WHERE status IN ('spawned', 'running');
CREATE INDEX IF NOT EXISTS idx_dispatches_scope ON dispatches(scope_id) WHERE scope_id IS NOT NULL;

-- v11: merge intent records (transactional outbox for git+SQLite coordination)
CREATE TABLE IF NOT EXISTS merge_intents (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    dispatch_id     TEXT NOT NULL,
    run_id          TEXT,
    base_commit     TEXT NOT NULL,
    patch_hash      TEXT,
    status          TEXT NOT NULL DEFAULT 'pending',
    result_commit   TEXT,
    conflict_files  TEXT,
    error_message   TEXT,
    created_at      INTEGER NOT NULL DEFAULT (unixepoch()),
    completed_at    INTEGER
);
CREATE INDEX IF NOT EXISTS idx_merge_intents_status ON merge_intents(status) WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS idx_merge_intents_dispatch ON merge_intents(dispatch_id);

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
    metadata        TEXT,
    phases          TEXT,
    token_budget    INTEGER,
    budget_warn_pct INTEGER DEFAULT 80,
    parent_run_id   TEXT,
    max_dispatches  INTEGER DEFAULT 0,
    budget_enforce  INTEGER DEFAULT 0,
    max_agents      INTEGER DEFAULT 0,
    gate_rules      TEXT
);
CREATE INDEX IF NOT EXISTS idx_runs_status ON runs(status) WHERE status = 'active';
CREATE INDEX IF NOT EXISTS idx_runs_parent ON runs(parent_run_id) WHERE parent_run_id IS NOT NULL;

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
    content_hash TEXT,
    dispatch_id TEXT,
    status      TEXT NOT NULL DEFAULT 'active',
    created_at  INTEGER NOT NULL DEFAULT (unixepoch())
);
CREATE INDEX IF NOT EXISTS idx_run_artifacts_run ON run_artifacts(run_id);
CREATE INDEX IF NOT EXISTS idx_run_artifacts_phase ON run_artifacts(run_id, phase);

-- v5: dispatch events (event bus)
CREATE TABLE IF NOT EXISTS dispatch_events (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    dispatch_id     TEXT NOT NULL,
    run_id          TEXT,
    from_status     TEXT NOT NULL,
    to_status       TEXT NOT NULL,
    event_type      TEXT NOT NULL DEFAULT 'status_change',
    reason          TEXT,
    created_at      INTEGER NOT NULL DEFAULT (unixepoch())
);
CREATE INDEX IF NOT EXISTS idx_dispatch_events_dispatch ON dispatch_events(dispatch_id);
CREATE INDEX IF NOT EXISTS idx_dispatch_events_run ON dispatch_events(run_id) WHERE run_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_dispatch_events_created ON dispatch_events(created_at);

-- v7: interspect evidence events
CREATE TABLE IF NOT EXISTS interspect_events (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id          TEXT,
    agent_name      TEXT NOT NULL,
    event_type      TEXT NOT NULL,
    override_reason TEXT,
    context_json    TEXT,
    session_id      TEXT,
    project_dir     TEXT,
    created_at      INTEGER NOT NULL DEFAULT (unixepoch())
);
CREATE INDEX IF NOT EXISTS idx_interspect_events_agent ON interspect_events(agent_name);
CREATE INDEX IF NOT EXISTS idx_interspect_events_created ON interspect_events(created_at);
CREATE INDEX IF NOT EXISTS idx_interspect_events_run ON interspect_events(run_id) WHERE run_id IS NOT NULL;

-- v9: discovery pipeline
CREATE TABLE IF NOT EXISTS discoveries (
    id              TEXT PRIMARY KEY,
    source          TEXT NOT NULL,
    source_id       TEXT NOT NULL,
    title           TEXT NOT NULL,
    summary         TEXT NOT NULL DEFAULT '',
    url             TEXT NOT NULL DEFAULT '',
    raw_metadata    TEXT NOT NULL DEFAULT '{}',
    embedding       BLOB,
    relevance_score REAL NOT NULL DEFAULT 0.0,
    confidence_tier TEXT NOT NULL DEFAULT 'low',
    status          TEXT NOT NULL DEFAULT 'new',
    run_id          TEXT,
    bead_id         TEXT,
    discovered_at   INTEGER NOT NULL DEFAULT (unixepoch()),
    promoted_at     INTEGER,
    reviewed_at     INTEGER,
    UNIQUE(source, source_id)
);
CREATE INDEX IF NOT EXISTS idx_discoveries_source ON discoveries(source);
CREATE INDEX IF NOT EXISTS idx_discoveries_status ON discoveries(status) WHERE status NOT IN ('dismissed');
CREATE INDEX IF NOT EXISTS idx_discoveries_tier ON discoveries(confidence_tier);

CREATE TABLE IF NOT EXISTS discovery_events (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    discovery_id    TEXT NOT NULL,
    event_type      TEXT NOT NULL,
    from_status     TEXT NOT NULL DEFAULT '',
    to_status       TEXT NOT NULL DEFAULT '',
    payload         TEXT NOT NULL DEFAULT '{}',
    created_at      INTEGER NOT NULL DEFAULT (unixepoch())
);
CREATE INDEX IF NOT EXISTS idx_discovery_events_discovery ON discovery_events(discovery_id);
CREATE INDEX IF NOT EXISTS idx_discovery_events_created ON discovery_events(created_at);

CREATE TABLE IF NOT EXISTS feedback_signals (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    discovery_id    TEXT NOT NULL,
    signal_type     TEXT NOT NULL,
    signal_data     TEXT NOT NULL DEFAULT '{}',
    actor           TEXT NOT NULL DEFAULT 'system',
    created_at      INTEGER NOT NULL DEFAULT (unixepoch())
);
CREATE INDEX IF NOT EXISTS idx_feedback_signals_discovery ON feedback_signals(discovery_id);

CREATE TABLE IF NOT EXISTS interest_profile (
    id              INTEGER PRIMARY KEY CHECK (id = 1),
    topic_vector    BLOB,
    keyword_weights TEXT NOT NULL DEFAULT '{}',
    source_weights  TEXT NOT NULL DEFAULT '{}',
    updated_at      INTEGER NOT NULL DEFAULT (unixepoch())
);

-- v10: portfolio orchestration
CREATE TABLE IF NOT EXISTS project_deps (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    portfolio_run_id    TEXT NOT NULL REFERENCES runs(id),
    upstream_project    TEXT NOT NULL,
    downstream_project  TEXT NOT NULL,
    created_at          INTEGER NOT NULL DEFAULT (unixepoch()),
    UNIQUE(portfolio_run_id, upstream_project, downstream_project)
);
CREATE INDEX IF NOT EXISTS idx_project_deps_portfolio ON project_deps(portfolio_run_id);

-- v13: thematic work lanes
CREATE TABLE IF NOT EXISTS lanes (
    id          TEXT NOT NULL PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    lane_type   TEXT NOT NULL DEFAULT 'standing',  -- 'standing' or 'arc'
    status      TEXT NOT NULL DEFAULT 'active',    -- 'active', 'closed', 'archived'
    description TEXT NOT NULL DEFAULT '',
    metadata    TEXT NOT NULL DEFAULT '{}',         -- JSON: pollard config, starvation weights
    created_at  INTEGER NOT NULL DEFAULT (unixepoch()),
    updated_at  INTEGER NOT NULL DEFAULT (unixepoch()),
    closed_at   INTEGER
);
CREATE INDEX IF NOT EXISTS idx_lanes_status ON lanes(status) WHERE status = 'active';
CREATE INDEX IF NOT EXISTS idx_lanes_type ON lanes(lane_type);

CREATE TABLE IF NOT EXISTS lane_events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    lane_id     TEXT NOT NULL REFERENCES lanes(id),
    event_type  TEXT NOT NULL,  -- 'created', 'bead_added', 'bead_removed', 'snapshot', 'closed'
    payload     TEXT NOT NULL DEFAULT '{}',
    created_at  INTEGER NOT NULL DEFAULT (unixepoch())
);
CREATE INDEX IF NOT EXISTS idx_lane_events_lane ON lane_events(lane_id);
CREATE INDEX IF NOT EXISTS idx_lane_events_created ON lane_events(created_at);

CREATE TABLE IF NOT EXISTS lane_members (
    lane_id     TEXT NOT NULL REFERENCES lanes(id),
    bead_id     TEXT NOT NULL,
    added_at    INTEGER NOT NULL DEFAULT (unixepoch()),
    PRIMARY KEY (lane_id, bead_id)
);
CREATE INDEX IF NOT EXISTS idx_lane_members_bead ON lane_members(bead_id);

-- v14: phase actions (event-driven advancement)
CREATE TABLE IF NOT EXISTS phase_actions (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id      TEXT NOT NULL REFERENCES runs(id),
    phase       TEXT NOT NULL,
    action_type TEXT NOT NULL DEFAULT 'command',
    command     TEXT NOT NULL,
    args        TEXT,
    mode        TEXT NOT NULL DEFAULT 'interactive',
    priority    INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL DEFAULT (unixepoch()),
    updated_at  INTEGER NOT NULL DEFAULT (unixepoch()),
    UNIQUE(run_id, phase, command)
);
CREATE INDEX IF NOT EXISTS idx_phase_actions_run ON phase_actions(run_id);
CREATE INDEX IF NOT EXISTS idx_phase_actions_phase ON phase_actions(run_id, phase);

-- v15: tamper-evident audit trail
CREATE TABLE IF NOT EXISTS audit_log (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id   TEXT NOT NULL,
    event_type   TEXT NOT NULL,
    actor        TEXT NOT NULL,
    target       TEXT NOT NULL DEFAULT '',
    payload      TEXT NOT NULL DEFAULT '{}',
    metadata     TEXT NOT NULL DEFAULT '{}',
    prev_hash    TEXT NOT NULL DEFAULT '',
    checksum     TEXT NOT NULL,
    sequence_num INTEGER NOT NULL,
    created_at   INTEGER NOT NULL DEFAULT (unixepoch())
);
CREATE INDEX IF NOT EXISTS idx_audit_log_session ON audit_log(session_id, sequence_num);
CREATE INDEX IF NOT EXISTS idx_audit_log_event_type ON audit_log(event_type);
CREATE INDEX IF NOT EXISTS idx_audit_log_actor ON audit_log(actor);
CREATE INDEX IF NOT EXISTS idx_audit_log_created ON audit_log(created_at);

-- v17: cost reconciliation records
CREATE TABLE IF NOT EXISTS cost_reconciliations (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id          TEXT NOT NULL,
    dispatch_id     TEXT,
    reported_in     INTEGER NOT NULL,
    reported_out    INTEGER NOT NULL,
    billed_in       INTEGER NOT NULL,
    billed_out      INTEGER NOT NULL,
    delta_in        INTEGER NOT NULL,
    delta_out       INTEGER NOT NULL,
    source          TEXT NOT NULL DEFAULT 'manual',
    created_at      INTEGER NOT NULL DEFAULT (unixepoch())
);
CREATE INDEX IF NOT EXISTS idx_cost_recon_run ON cost_reconciliations(run_id);
CREATE INDEX IF NOT EXISTS idx_cost_recon_dispatch ON cost_reconciliations(dispatch_id) WHERE dispatch_id IS NOT NULL;
