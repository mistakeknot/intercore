CREATE TABLE IF NOT EXISTS authorizations (
  id               TEXT PRIMARY KEY,
  op_type          TEXT NOT NULL,
  target           TEXT NOT NULL,
  agent_id         TEXT NOT NULL CHECK(length(trim(agent_id)) > 0),
  bead_id          TEXT,
  mode             TEXT NOT NULL CHECK(mode IN ('auto','confirmed','blocked','force_auto')),
  policy_match     TEXT,
  policy_hash      TEXT,
  vetted_sha       TEXT,
  vetting          TEXT CHECK(vetting IS NULL OR json_valid(vetting)),
  cross_project_id TEXT,
  created_at       INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS authz_by_bead  ON authorizations(bead_id,  created_at DESC);
CREATE INDEX IF NOT EXISTS authz_by_op    ON authorizations(op_type,  created_at DESC);
CREATE INDEX IF NOT EXISTS authz_by_agent ON authorizations(agent_id, created_at DESC);
CREATE INDEX IF NOT EXISTS authz_by_xproj ON authorizations(cross_project_id) WHERE cross_project_id IS NOT NULL;
