-- Migration 034 — authz_tokens: unforgeable token protocol with delegation chain.
--
-- See docs/canon/authz-token-model.md for the normative semantics (lifecycle,
-- scope, delegation, consume, revoke).
-- See docs/canon/authz-token-payload.md for the canonical signed payload
-- (sig_version=2, distinct from v1.5's sig_version=1).
--
-- v2 ships the table + indexes + a cutover marker (lives in `authorizations`
-- as a v1.5-shaped row so `policy audit` surfaces the boundary). Real DDL
-- runs inline in db.go under `if currentVersion >= 33 && currentVersion < 34`.
-- This file is documentation only since migrations ≥021.

CREATE TABLE IF NOT EXISTS authz_tokens (
    id            TEXT PRIMARY KEY,                 -- ULID (Crockford base32, 26 chars)
    op_type       TEXT NOT NULL,
    target        TEXT NOT NULL,
    agent_id      TEXT NOT NULL CHECK(length(trim(agent_id)) > 0),  -- who may present
    bead_id       TEXT,                             -- optional scope to a bead
    delegate_to   TEXT,                             -- NULL for roots; recipient agent id on delegations
    expires_at    INTEGER NOT NULL,                 -- unix seconds
    consumed_at   INTEGER,                          -- NULL until atomic consume; set exactly once
    revoked_at    INTEGER,                          -- NULL unless revoked; set exactly once per revoke
    issued_by     TEXT NOT NULL,                    -- agent id of the issuer, or "user"
    parent_token  TEXT REFERENCES authz_tokens(id) ON DELETE RESTRICT,
    root_token    TEXT,                             -- first ancestor; NULL for roots; denormalized for O(1) cascade
    depth         INTEGER NOT NULL DEFAULT 0 CHECK (depth >= 0 AND depth <= 3),
    sig_version   INTEGER NOT NULL DEFAULT 2,       -- distinct from v1.5's sig_version=1
    signature     BLOB NOT NULL,                    -- Ed25519 over CanonicalTokenPayload(row)
    created_at    INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS tokens_by_root
    ON authz_tokens(root_token, consumed_at, revoked_at);
CREATE INDEX IF NOT EXISTS tokens_by_parent
    ON authz_tokens(parent_token);
CREATE INDEX IF NOT EXISTS tokens_by_expiry
    ON authz_tokens(expires_at) WHERE consumed_at IS NULL AND revoked_at IS NULL;
CREATE INDEX IF NOT EXISTS tokens_by_agent
    ON authz_tokens(agent_id, created_at DESC);

-- Cutover marker: lives in `authorizations` (v1.5-shaped), not `authz_tokens`.
-- Fixed id + INSERT OR IGNORE → idempotent across migration re-runs. Mirrors
-- the v1.5 migration-033 marker pattern.
INSERT OR IGNORE INTO authorizations (
    id,
    op_type,
    target,
    agent_id,
    mode,
    created_at,
    sig_version
) VALUES (
    'migration-034-tokens-enabled',
    'migration.tokens-enabled',
    'authz_tokens',
    'system:migration-034',
    'auto',
    CAST(strftime('%s','now') AS INTEGER),
    1
);
