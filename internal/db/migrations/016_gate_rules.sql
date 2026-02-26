-- Migration 016: runtime-configurable gate rules
ALTER TABLE runs ADD COLUMN gate_rules TEXT;
