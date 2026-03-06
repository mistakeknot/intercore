-- v27: routing decision records (iv-godia)
-- Persists routing decisions as replayable kernel facts for offline
-- counterfactual evaluation. Each row captures the full resolution trace:
-- input context, which rule matched, safety floor effects, and candidate sets.

CREATE TABLE IF NOT EXISTS routing_decisions (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    dispatch_id     TEXT,
    run_id          TEXT,
    session_id      TEXT,
    bead_id         TEXT,
    project_dir     TEXT NOT NULL,
    phase           TEXT,
    agent           TEXT NOT NULL,
    category        TEXT,
    selected_model  TEXT NOT NULL,
    rule_matched    TEXT NOT NULL,
    floor_applied   INTEGER NOT NULL DEFAULT 0,
    floor_from      TEXT,
    floor_to        TEXT,
    candidates      TEXT,
    excluded        TEXT,
    policy_hash     TEXT,
    override_id     TEXT,
    complexity      INTEGER,
    context_json    TEXT,
    decided_at      INTEGER NOT NULL DEFAULT (unixepoch())
);
CREATE INDEX IF NOT EXISTS idx_routing_dec_project ON routing_decisions(project_dir, decided_at DESC);
CREATE INDEX IF NOT EXISTS idx_routing_dec_agent ON routing_decisions(agent, decided_at DESC);
CREATE INDEX IF NOT EXISTS idx_routing_dec_model ON routing_decisions(selected_model, decided_at DESC);
CREATE INDEX IF NOT EXISTS idx_routing_dec_dispatch ON routing_decisions(dispatch_id) WHERE dispatch_id IS NOT NULL;
