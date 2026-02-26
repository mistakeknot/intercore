-- Migration 017: cost reconciliation records
CREATE TABLE IF NOT EXISTS cost_reconciliations (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id          TEXT NOT NULL,
    dispatch_id     TEXT,
    reported_in     INTEGER NOT NULL,
    reported_out    INTEGER NOT NULL,
    billed_in       INTEGER NOT NULL,
    billed_out      INTEGER NOT NULL,
    delta_in        INTEGER NOT NULL,
    delta_out       INTEGER NOT NULL,
    source          TEXT NOT NULL DEFAULT 'manual',
    created_at      INTEGER NOT NULL DEFAULT (unixepoch())
);
CREATE INDEX IF NOT EXISTS idx_cost_recon_run ON cost_reconciliations(run_id);
CREATE INDEX IF NOT EXISTS idx_cost_recon_dispatch ON cost_reconciliations(dispatch_id) WHERE dispatch_id IS NOT NULL;
