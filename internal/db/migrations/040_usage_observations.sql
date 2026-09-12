-- v40: provider-neutral usage evidence and validation snapshots.
-- Both ledgers are append-only. Corrections and later bindings append a new
-- observation linked through supersedes; validation never rewrites evidence.

CREATE TABLE IF NOT EXISTS usage_observations (
    id                  TEXT NOT NULL PRIMARY KEY,
    provider            TEXT NOT NULL,
    source              TEXT NOT NULL,
    kind                TEXT NOT NULL CHECK(kind IN ('rate_limits','account_usage','local_usage')),
    status              TEXT NOT NULL CHECK(status IN ('available','partial','unavailable','error')),
    reason              TEXT,
    captured_at         TEXT NOT NULL,
    source_at           TEXT,
    interval_start      INTEGER,
    interval_end        INTEGER,
    quota_bucket        TEXT,
    quota_reset_at      TEXT,
    counters_json       TEXT NOT NULL CHECK(json_valid(counters_json)),
    identity_json       TEXT NOT NULL CHECK(json_valid(identity_json)),
    execution_refs_json TEXT NOT NULL CHECK(json_valid(execution_refs_json)),
    payload_sha256      TEXT NOT NULL CHECK(length(payload_sha256) = 64),
    supersedes          TEXT REFERENCES usage_observations(id) ON DELETE RESTRICT,
    canonical_input     BLOB NOT NULL,
    canonical_sha256    TEXT NOT NULL UNIQUE CHECK(length(canonical_sha256) = 64),
    inserted_at         INTEGER NOT NULL,
    CHECK((interval_start IS NULL) = (interval_end IS NULL)),
    CHECK(interval_start IS NULL OR interval_start <= interval_end),
    CHECK(supersedes IS NULL OR supersedes <> id)
);
CREATE INDEX IF NOT EXISTS idx_usage_observations_source_time
    ON usage_observations(provider, source, kind, captured_at);
CREATE INDEX IF NOT EXISTS idx_usage_observations_interval
    ON usage_observations(interval_start, interval_end)
    WHERE interval_start IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_usage_observations_supersedes
    ON usage_observations(supersedes) WHERE supersedes IS NOT NULL;

CREATE TABLE IF NOT EXISTS usage_validations (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    observation_id  TEXT NOT NULL REFERENCES usage_observations(id) ON DELETE RESTRICT,
    evaluated_at    INTEGER NOT NULL,
    max_age_seconds INTEGER NOT NULL CHECK(max_age_seconds > 0),
    evidence_json   TEXT NOT NULL CHECK(json_valid(evidence_json))
);
CREATE INDEX IF NOT EXISTS idx_usage_validations_observation
    ON usage_validations(observation_id, evaluated_at, id);

CREATE TRIGGER IF NOT EXISTS usage_observations_no_update
BEFORE UPDATE ON usage_observations
BEGIN
    SELECT RAISE(ABORT, 'usage_observations is append-only');
END;

CREATE TRIGGER IF NOT EXISTS usage_observations_no_delete
BEFORE DELETE ON usage_observations
BEGIN
    SELECT RAISE(ABORT, 'usage_observations is append-only');
END;

CREATE TRIGGER IF NOT EXISTS usage_validations_no_update
BEFORE UPDATE ON usage_validations
BEGIN
    SELECT RAISE(ABORT, 'usage_validations is append-only');
END;

CREATE TRIGGER IF NOT EXISTS usage_validations_no_delete
BEFORE DELETE ON usage_validations
BEGIN
    SELECT RAISE(ABORT, 'usage_validations is append-only');
END;
