-- v28: add event_type to review_events for execution_defect support
ALTER TABLE review_events ADD COLUMN event_type TEXT NOT NULL DEFAULT 'disagreement_resolved';
CREATE INDEX IF NOT EXISTS idx_review_events_type ON review_events(event_type);
