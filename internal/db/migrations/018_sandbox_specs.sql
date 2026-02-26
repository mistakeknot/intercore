-- Migration 018: sandbox specification columns
ALTER TABLE dispatches ADD COLUMN sandbox_spec TEXT;
ALTER TABLE dispatches ADD COLUMN sandbox_effective TEXT;
