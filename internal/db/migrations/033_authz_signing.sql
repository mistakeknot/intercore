-- Migration 033 — Add Ed25519 signing columns to authorizations + cutover marker.
--
-- See docs/canon/authz-signing-trust-model.md for the trust model.
-- See docs/canon/authz-signing-payload.md for the canonical signed payload.
--
-- sig_version default 0 marks pre-v1.5 vintage rows (NULL signature = never
-- signed, not = tampered). The cutover marker row below is sig_version=1 by
-- default (once signed by the first `policy sign` invocation); until then
-- it is a distinguishable anchor because its op_type is unique.

ALTER TABLE authorizations ADD COLUMN sig_version INTEGER NOT NULL DEFAULT 0;
ALTER TABLE authorizations ADD COLUMN signature   BLOB;
ALTER TABLE authorizations ADD COLUMN signed_at   INTEGER;

-- Cutover marker: its created_at demarcates pre-v1.5 rows (retroactively
-- "pre-signing vintage") from rows written on or after v1.5. A fixed ID
-- makes re-application of the migration idempotent.
INSERT OR IGNORE INTO authorizations (
  id,
  op_type,
  target,
  agent_id,
  mode,
  created_at,
  sig_version
) VALUES (
  'migration-033-cutover-marker',
  'migration.signing-enabled',
  'authorizations',
  'system:migration-033',
  'auto',
  CAST(strftime('%s','now') AS INTEGER),
  1
);

-- Fast lookup for the signer: rows needing signature (post-cutover, unsigned).
CREATE INDEX IF NOT EXISTS authz_unsigned
  ON authorizations(sig_version, signed_at)
  WHERE signature IS NULL AND sig_version >= 1;
