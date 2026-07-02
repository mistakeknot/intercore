-- v30: token tracking on sessions (Demarch-sgrv)
-- Add per-session aggregate token counts for billing and context tracking.
-- Follows the dual-metric pattern: billing (input+output) vs effective context
-- (input+cache_read+cache_creation). See docs/solutions/patterns/token-accounting-billing-vs-context.md

ALTER TABLE sessions ADD COLUMN input_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN output_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN cache_creation_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN cache_read_tokens INTEGER NOT NULL DEFAULT 0;
