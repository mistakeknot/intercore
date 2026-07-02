-- Migration 035 — action_receipts: signed HMAC-SHA256 receipt store.
--
-- See docs/canon/signed-receipts-v1.md for the normative schema, canonicalization
-- rules, and trust model.
--
-- v1 storage substrate is SQLite (amended from "Dolt" in the original canon
-- §Storage section — see canon §v1 → v1.1 migration for the v1.1 Dolt port).
-- INSERT-only is enforced by SQLite triggers (no equivalent of a Dolt schema
-- DELETE-grant denial; triggers RAISE on UPDATE/DELETE).
--
-- Real DDL runs inline in db.go under `if currentVersion >= 34 && currentVersion < 35`.
-- This file is documentation only since migrations ≥021.
--
-- 8 signed fields map to typed columns (mirrors receipt.Receipt). The unsigned
-- envelope is stored alongside. payload_canonical holds the exact bytes the
-- HMAC was computed over so verifiers do not have to re-canonicalize.
--
-- tool_calls is stored as canonical JSON text (a fragment of payload_canonical)
-- rather than a join table. v1 reads receipts whole; v2 may normalize if
-- query patterns demand.

CREATE TABLE IF NOT EXISTS action_receipts (
    receipt_id        TEXT PRIMARY KEY,                       -- rcpt_<26-char ULID>
    timestamp         TEXT NOT NULL,                          -- canon RFC3339-microsecond
    agent_id          TEXT NOT NULL,
    model             TEXT NOT NULL,
    tool_calls_json   TEXT NOT NULL,                          -- canonical JSON array fragment
    parent_run_id     TEXT,                                   -- nullable per canon
    content_hash      TEXT NOT NULL,                          -- SHA-256 hex of action output
    schema_version    INTEGER NOT NULL,
    signature         TEXT NOT NULL,                          -- 64-char hex HMAC-SHA256
    signature_alg     TEXT NOT NULL,                          -- "hmac-sha256-v1"
    key_id            TEXT NOT NULL,                          -- "<agent_id>#<rotation_epoch>"
    signed_at         TEXT NOT NULL,                          -- canon RFC3339-microsecond
    payload_canonical BLOB NOT NULL,                          -- exact HMAC input bytes
    inserted_at       INTEGER NOT NULL                        -- unix seconds, row-write time
);

-- Read patterns: by-agent timeline (ic receipt list --agent X --since 7d),
-- by-parent causal walks (verify chain integrity).
CREATE INDEX IF NOT EXISTS receipts_by_agent_time
    ON action_receipts(agent_id, timestamp);
CREATE INDEX IF NOT EXISTS receipts_by_parent
    ON action_receipts(parent_run_id) WHERE parent_run_id IS NOT NULL;

-- INSERT-only: any DELETE or UPDATE aborts the transaction with an explicit
-- error referencing the canon doc. This is the SQLite analogue of canon §Trust
-- claim "Receipt-row deletion is forbidden by schema."
CREATE TRIGGER IF NOT EXISTS action_receipts_no_delete
BEFORE DELETE ON action_receipts
BEGIN
    SELECT RAISE(ABORT, 'action_receipts is INSERT-only per docs/canon/signed-receipts-v1.md');
END;

CREATE TRIGGER IF NOT EXISTS action_receipts_no_update
BEFORE UPDATE ON action_receipts
BEGIN
    SELECT RAISE(ABORT, 'action_receipts is INSERT-only per docs/canon/signed-receipts-v1.md');
END;

-- v35 cutover marker — lives in `authorizations` (v1.5-shape) so `policy audit`
-- surfaces the boundary without joining across tables, matching the pattern
-- established by v33 and v34.
INSERT OR IGNORE INTO authorizations (
    id, op_type, target, agent_id, mode, created_at, sig_version
) VALUES (
    'migration-035-receipts-enabled',
    'migration.receipts-enabled',
    'action_receipts',
    'system:migration-035',
    'auto',
    CAST(strftime('%s','now') AS INTEGER),
    1
);
