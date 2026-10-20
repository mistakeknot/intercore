-- v31: lane strategic intent
-- First-class intent field on lanes for autonomous epic execution.
-- Intent is injected into sprint briefings so agents maintain strategic
-- context across delegation layers.

ALTER TABLE lanes ADD COLUMN intent TEXT NOT NULL DEFAULT '';
