-- v40: dispatch terminal records (mk-qozg; contract
-- docs/plans/2026-09-14-mk-qozg-completion-contract.md §4).
-- One immutable terminal record per dispatch, written in the same transaction
-- as the status change and its terminal event. Supervision state, cancel
-- intents, consumption acknowledgements and delivery audit rows serve the
-- non-polling await. The triggers cover status writes that bypass the terminal
-- writer: they still record the terminal (marked unrecorded, so it never
-- counts as evidence), refuse to let a supervised dispatch become terminal
-- without its record, and keep records immutable. The outbox trigger stamps
-- created_at with unixepoch(), a documented exception to Go-side timestamps.
CREATE TABLE IF NOT EXISTS dispatch_terminals (
    dispatch_id          TEXT NOT NULL PRIMARY KEY,
    run_id               TEXT NOT NULL DEFAULT '',
    attempt              INTEGER NOT NULL,
    parent_dispatch_id   TEXT NOT NULL DEFAULT '',
    status               TEXT NOT NULL CHECK (status IN ('completed','failed','timeout','cancelled')),
    from_status          TEXT NOT NULL,
    terminal_source      TEXT NOT NULL CHECK (terminal_source IN ('supervisor','reconcile','collect','spawn','cancel_by_run','kill','spool','trigger','legacy')),
    exit_code            INTEGER,
    exit_signal          TEXT,
    failure_class        TEXT,
    evidence_status      TEXT NOT NULL CHECK (evidence_status IN ('verified','missing','malformed','mismatched','not_applicable','unrecorded')),
    usage_status         TEXT NOT NULL DEFAULT 'unknown' CHECK (usage_status IN ('complete','incomplete','unknown')),
    input_tokens         INTEGER,
    output_tokens        INTEGER,
    cache_read_tokens    INTEGER,
    cache_write_tokens   INTEGER,
    producer_backend     TEXT,
    producer_model       TEXT,
    route_decision_id    INTEGER,
    report_path          TEXT,
    report_sha256        TEXT,
    artifacts_json       TEXT NOT NULL DEFAULT '{}',
    base_commit          TEXT,
    base_worktree_digest TEXT,
    head_commit          TEXT,
    head_worktree_digest TEXT,
    orphans_killed       INTEGER NOT NULL DEFAULT 0,
    contested            INTEGER NOT NULL DEFAULT 0,
    event_id             INTEGER NOT NULL,
    created_at           INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS dispatch_supervision (
    dispatch_id          TEXT NOT NULL PRIMARY KEY,
    db_path              TEXT NOT NULL,
    supervisor_pid       INTEGER,
    supervisor_birth     TEXT,
    worker_pid           INTEGER,
    worker_birth         TEXT,
    prompt_sha256        TEXT NOT NULL,
    base_commit          TEXT,
    base_worktree_digest TEXT,
    report_path          TEXT,
    observe_lock_path    TEXT,
    deadline_unix        INTEGER,
    created_at           INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS dispatch_intents (
    id                   INTEGER PRIMARY KEY AUTOINCREMENT,
    dispatch_id          TEXT NOT NULL,
    kind                 TEXT NOT NULL CHECK (kind IN ('cancel','run_rollback','deadline')),
    reason               TEXT,
    requested_by         TEXT,
    created_at           INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS dispatch_consumptions (
    consumer             TEXT NOT NULL,
    dispatch_id          TEXT NOT NULL,
    run_id               TEXT NOT NULL DEFAULT '',
    stage_key            TEXT NOT NULL DEFAULT '',
    effect_kind          TEXT,
    effect_ref           TEXT,
    superseded_by        TEXT,
    supersede_reason     TEXT,
    created_at           INTEGER NOT NULL,
    PRIMARY KEY (consumer, dispatch_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_consumptions_stage ON dispatch_consumptions(consumer, run_id, stage_key)
    WHERE stage_key <> '' AND superseded_by IS NULL;
CREATE TABLE IF NOT EXISTS dispatch_terminal_deliveries (
    id                   INTEGER PRIMARY KEY AUTOINCREMENT,
    consumer             TEXT NOT NULL,
    dispatch_id          TEXT NOT NULL,
    outcome              TEXT NOT NULL,
    created_at           INTEGER NOT NULL
);

-- Safety net: a non-terminal to terminal status write with no terminal record
-- gets a terminal event and a terminal record here.
CREATE TRIGGER IF NOT EXISTS dispatches_terminal_outbox
AFTER UPDATE OF status ON dispatches
WHEN OLD.status NOT IN ('completed','failed','timeout','cancelled')
 AND NEW.status IN ('completed','failed','timeout','cancelled')
 AND NOT EXISTS (SELECT 1 FROM dispatch_terminals WHERE dispatch_id = NEW.id)
BEGIN
    INSERT INTO dispatch_events (dispatch_id, run_id, from_status, to_status, event_type, reason, created_at)
    VALUES (NEW.id, NEW.scope_id, OLD.status, NEW.status, 'terminal', 'status written without a terminal record', unixepoch());
    INSERT INTO dispatch_terminals (dispatch_id, run_id, attempt, parent_dispatch_id, status, from_status,
        terminal_source, exit_code, evidence_status, event_id, created_at)
    VALUES (NEW.id, COALESCE(NEW.scope_id, ''), NEW.retry_count, NEW.parent_dispatch_id, NEW.status, OLD.status,
        'trigger', NEW.exit_code, 'unrecorded', last_insert_rowid(), unixepoch());
END;

-- A supervised dispatch becomes terminal only through its terminal record,
-- which the terminal writer inserts before the status update.
CREATE TRIGGER IF NOT EXISTS dispatches_supervision_guard
BEFORE UPDATE OF status ON dispatches
WHEN OLD.status NOT IN ('completed','failed','timeout','cancelled')
 AND NEW.status IN ('completed','failed','timeout','cancelled')
 AND EXISTS (SELECT 1 FROM dispatch_supervision WHERE dispatch_id = NEW.id)
 AND NOT EXISTS (SELECT 1 FROM dispatch_terminals WHERE dispatch_id = NEW.id)
BEGIN
    SELECT RAISE(ABORT, 'supervised dispatch cannot become terminal without its terminal record');
END;

CREATE TRIGGER IF NOT EXISTS dispatch_terminals_no_update
BEFORE UPDATE ON dispatch_terminals
BEGIN
    SELECT RAISE(ABORT, 'dispatch terminal records are immutable');
END;

-- Erratum 3.1: a terminal record goes only after its dispatch row, the order
-- prune deletes in.
CREATE TRIGGER IF NOT EXISTS dispatch_terminals_no_delete
BEFORE DELETE ON dispatch_terminals
WHEN EXISTS (SELECT 1 FROM dispatches WHERE id = OLD.dispatch_id)
BEGIN
    SELECT RAISE(ABORT, 'dispatch terminal records are deleted only with their dispatch');
END;
