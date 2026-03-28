-- v31: lane strategic intent (sylveste-rsj.1.1)
-- First-class intent field on lanes for autonomous epic execution.
-- Intent is injected into sprint briefings so agents maintain strategic
-- context across delegation layers. See docs/brainstorms/2026-03-28-autonomous-epic-execution-brainstorm.md

ALTER TABLE lanes ADD COLUMN intent TEXT NOT NULL DEFAULT '';
