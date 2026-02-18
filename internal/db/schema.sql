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
