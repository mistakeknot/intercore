-- Migration 041: scheduler runner state (claim tokens, lease epoch, attempts).
--
-- Chunked: the migrator splits this file on the "-- ic:chunk <kind>" markers
-- and runs each chunk separately so a duplicate-column error in one ALTER does
-- not skip the rest of the file. DB.Migrate runs the alter chunks first (the
-- table-creating schema.sql DDL then adds the rest), skips the ddl chunk (the
-- same objects live in schema.sql), then runs the data chunks. Every chunk is
-- idempotent.

-- ic:chunk alter
ALTER TABLE scheduler_jobs ADD COLUMN idempotency_key TEXT;
-- ic:chunk alter
ALTER TABLE scheduler_jobs ADD COLUMN not_before INTEGER NOT NULL DEFAULT 0;
-- ic:chunk alter
ALTER TABLE scheduler_jobs ADD COLUMN attempt_seq INTEGER NOT NULL DEFAULT 0;
-- ic:chunk alter
ALTER TABLE scheduler_jobs ADD COLUMN crash_requeues INTEGER NOT NULL DEFAULT 0;
-- ic:chunk alter
ALTER TABLE scheduler_jobs ADD COLUMN claim_token TEXT;
-- ic:chunk alter
ALTER TABLE scheduler_jobs ADD COLUMN lease_epoch INTEGER;
-- ic:chunk alter
ALTER TABLE scheduler_jobs ADD COLUMN retry_policy TEXT;
-- ic:chunk alter
ALTER TABLE scheduler_jobs ADD COLUMN idempotent INTEGER NOT NULL DEFAULT 0;
-- ic:chunk alter
ALTER TABLE scheduler_jobs ADD COLUMN outcome_class TEXT;
-- ic:chunk alter
ALTER TABLE scheduler_jobs ADD COLUMN output_base TEXT;
-- ic:chunk alter
ALTER TABLE scheduler_jobs ADD COLUMN project_dir TEXT;
-- ic:chunk alter
ALTER TABLE scheduler_jobs ADD COLUMN retry_blocked TEXT;
-- ic:chunk alter
ALTER TABLE scheduler_jobs ADD COLUMN updated_at INTEGER;

-- ic:chunk ddl
CREATE UNIQUE INDEX IF NOT EXISTS idx_scheduler_jobs_idempotency
    ON scheduler_jobs(idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_scheduler_jobs_claim
    ON scheduler_jobs(status, not_before, priority, created_at);

CREATE TABLE IF NOT EXISTS scheduler_runner (
    id            INTEGER PRIMARY KEY CHECK (id = 1),
    epoch         INTEGER NOT NULL DEFAULT 1,
    runner_id     TEXT,
    pid           INTEGER,
    pid_starttime INTEGER,
    boot_id       TEXT,
    host          TEXT,
    started_at    INTEGER,
    heartbeat_at  INTEGER,
    version       TEXT,
    state         TEXT NOT NULL DEFAULT 'stopped' CHECK (state IN ('running','draining','stopped')),
    db_identity   TEXT,
    db_path       TEXT,
    queue_id      TEXT NOT NULL,
    uid           INTEGER,
    net_ns        INTEGER
);

CREATE TABLE IF NOT EXISTS scheduler_job_attempts (
    job_id              TEXT NOT NULL REFERENCES scheduler_jobs(id) ON DELETE CASCADE,
    attempt_no          INTEGER NOT NULL,
    claim_token         TEXT NOT NULL,
    lease_epoch         INTEGER NOT NULL DEFAULT 0,
    dispatch_id         TEXT REFERENCES dispatches(id) ON DELETE SET NULL,
    linked_at           INTEGER,
    output_file         TEXT,
    cgroup_path         TEXT,
    containment         TEXT NOT NULL DEFAULT 'none' CHECK (containment IN ('cgroup','none')),
    claimed_at          INTEGER NOT NULL,
    launch_result       TEXT CHECK (launch_result IS NULL OR launch_result IN ('rejected','hook_aborted','spawn_error','started')),
    exited_at           INTEGER,
    ended_at            INTEGER,
    evidence_class      TEXT,
    evidence_sha256     TEXT,
    evidence_source     TEXT,
    outcome_class       TEXT,
    counts_toward_limit INTEGER NOT NULL DEFAULT 1 CHECK (counts_toward_limit IN (0,1)),
    legacy              INTEGER NOT NULL DEFAULT 0 CHECK (legacy IN (0,1)),
    error_msg           TEXT,
    PRIMARY KEY (job_id, attempt_no),
    UNIQUE (claim_token)
);
CREATE INDEX IF NOT EXISTS idx_scheduler_job_attempts_dispatch
    ON scheduler_job_attempts(dispatch_id) WHERE dispatch_id IS NOT NULL;

-- ic:chunk data
-- The singleton runner row carries the queue identity: 128 random bits,
-- generated once. A first-upgrade (040) backup has no such row, so restoring
-- it and re-running this migration mints a fresh queue_id.
INSERT OR IGNORE INTO scheduler_runner (id, epoch, queue_id, state)
VALUES (1, 1, lower(hex(randomblob(16))), 'stopped');

-- ic:chunk data
-- Leave the queue paused when legacy rows are in flight or will be
-- quarantined, so nothing runs automatically after the upgrade. A queue with
-- only terminal rows keeps whatever pause state it had. Rows are recognised by
-- updated_at IS NULL (not yet normalized), so a re-run is a no-op.
INSERT INTO state (key, scope_id, payload, updated_at)
SELECT 'scheduler_paused', 'global', 'true', unixepoch()
WHERE EXISTS (
    SELECT 1 FROM scheduler_jobs
    WHERE updated_at IS NULL AND status IN ('pending','scheduled','running','retrying')
)
ON CONFLICT(key, scope_id) DO UPDATE SET payload = 'true', updated_at = unixepoch();

-- ic:chunk data
-- Backfill project_dir and output_base from spawn_opts (absolute values
-- only), and a conservative retry policy: legacy rows have no idempotency
-- flag, so they get at most one attempt. The executor stays in
-- spawn_opts.DispatchSH; there is no executor column.
UPDATE scheduler_jobs SET
    project_dir = CASE WHEN json_valid(spawn_opts)
        THEN CASE WHEN json_extract(spawn_opts, '$.ProjectDir') LIKE '/%'
            THEN json_extract(spawn_opts, '$.ProjectDir') END
        END,
    output_base = CASE WHEN json_valid(spawn_opts)
        THEN CASE WHEN json_extract(spawn_opts, '$.OutputFile') LIKE '/%'
            THEN json_extract(spawn_opts, '$.OutputFile') END
        END,
    retry_policy = json_object('max_attempts', 1)
WHERE updated_at IS NULL;

-- ic:chunk data
-- Legacy in-flight rows that name a dispatch become one synthetic attempt and
-- a running job with a synthetic token (epoch 0: older than every runner, so
-- recovery owns them and never adopts them). dispatch_id comes from a subselect
-- so a dispatch row that no longer exists leaves it NULL.
INSERT INTO scheduler_job_attempts
    (job_id, attempt_no, claim_token, lease_epoch, dispatch_id, linked_at, claimed_at,
     containment, counts_toward_limit, legacy)
SELECT j.id, 1, 'legacy-' || j.id, 0,
       (SELECT d.id FROM dispatches d WHERE d.id = j.dispatch_id),
       COALESCE(j.started_at, j.created_at),
       COALESCE(j.started_at, j.created_at),
       'none', 1, 1
FROM scheduler_jobs j
WHERE j.updated_at IS NULL
  AND j.status IN ('pending','scheduled','running','retrying')
  AND j.dispatch_id IS NOT NULL;

-- ic:chunk data
UPDATE scheduler_jobs SET
    status = 'running',
    claim_token = 'legacy-' || id,
    lease_epoch = 0,
    attempt_seq = 1,
    updated_at = unixepoch()
WHERE updated_at IS NULL
  AND status IN ('pending','scheduled','running','retrying')
  AND dispatch_id IS NOT NULL;

-- ic:chunk data
-- Non-terminal legacy rows with no dispatch cannot be proven not-started.
-- Quarantine them; an operator requeues or cancels.
UPDATE scheduler_jobs SET
    status = 'quarantined',
    outcome_class = 'legacy_unknown',
    updated_at = unixepoch()
WHERE updated_at IS NULL
  AND status IN ('pending','scheduled','running','retrying');

-- ic:chunk data
UPDATE scheduler_jobs SET
    updated_at = COALESCE(completed_at, started_at, created_at)
WHERE updated_at IS NULL;
