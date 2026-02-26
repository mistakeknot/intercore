-- Migration 019: scheduler job queue
CREATE TABLE IF NOT EXISTS scheduler_jobs (
    id          TEXT PRIMARY KEY,
    status      TEXT NOT NULL DEFAULT 'pending',
    priority    INTEGER NOT NULL DEFAULT 2,
    agent_type  TEXT NOT NULL DEFAULT 'codex',
    session_name TEXT,
    batch_id    TEXT,
    dispatch_id TEXT,
    spawn_opts  TEXT NOT NULL,
    max_retries INTEGER NOT NULL DEFAULT 3,
    retry_count INTEGER NOT NULL DEFAULT 0,
    error_msg   TEXT,
    created_at  INTEGER NOT NULL,
    started_at  INTEGER,
    completed_at INTEGER,
    FOREIGN KEY (dispatch_id) REFERENCES dispatches(id)
);
CREATE INDEX IF NOT EXISTS idx_scheduler_jobs_status ON scheduler_jobs(status);
CREATE INDEX IF NOT EXISTS idx_scheduler_jobs_session ON scheduler_jobs(session_name);
