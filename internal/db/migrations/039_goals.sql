CREATE TABLE IF NOT EXISTS goals (
    id                     TEXT NOT NULL PRIMARY KEY,
    project_dir            TEXT NOT NULL,
    title                  TEXT NOT NULL,
    charter_path           TEXT,
    condition_text         TEXT NOT NULL DEFAULT '',
    status                 TEXT NOT NULL DEFAULT 'open',
    complexity             INTEGER NOT NULL DEFAULT 3,
    fence_gen              INTEGER NOT NULL DEFAULT 0,
    closing_run_id         TEXT,
    lease_owner            TEXT,
    lease_expires_at       INTEGER,
    verified_at            INTEGER,
    reflected_at           INTEGER,
    compounded_at          INTEGER,
    successor_proposed_at  INTEGER,
    successor_ref          TEXT,
    last_run_advanced_at   INTEGER,
    bead_id                TEXT,
    created_at             INTEGER NOT NULL DEFAULT (unixepoch()),
    updated_at             INTEGER NOT NULL DEFAULT (unixepoch()),
    amended_at             INTEGER,
    closed_at              INTEGER
);
CREATE INDEX IF NOT EXISTS idx_goals_live ON goals(status) WHERE status IN ('open','closing');
CREATE INDEX IF NOT EXISTS idx_goals_project ON goals(project_dir, status);
ALTER TABLE runs ADD COLUMN goal_id TEXT;
CREATE INDEX IF NOT EXISTS idx_runs_goal ON runs(goal_id) WHERE goal_id IS NOT NULL;
