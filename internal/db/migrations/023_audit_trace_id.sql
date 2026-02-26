-- v23: add trace_id to audit_log for cross-layer trace correlation
ALTER TABLE audit_log ADD COLUMN trace_id TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_audit_log_trace ON audit_log(trace_id) WHERE trace_id != '';
