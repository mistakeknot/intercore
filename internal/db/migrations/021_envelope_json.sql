-- Migration 021: event envelope metadata columns (provenance/capability/trace)
ALTER TABLE phase_events ADD COLUMN envelope_json TEXT;
ALTER TABLE dispatch_events ADD COLUMN envelope_json TEXT;
ALTER TABLE coordination_events ADD COLUMN envelope_json TEXT;
