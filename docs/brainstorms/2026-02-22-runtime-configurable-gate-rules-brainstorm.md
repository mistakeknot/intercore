# Runtime-Configurable Gate Rules
**Bead:** iv-yfck
**Phase:** brainstorm (as of 2026-02-23T07:50:59Z)

## What We're Building

Move gate rules from a hardcoded Go map to per-run runtime configuration. Runs created with `ic run create` can supply custom gate rules via `--gates=JSON` or `--gates-file=path`. Runs without custom gates use the existing hardcoded defaults.

## Why This Approach

The current `gateRules` map in `gate.go` is compile-time fixed. Agency specs already inject `SpecRules` at call-time, but these aren't persisted per-run — they're loaded transiently. For the kernel to be a general-purpose orchestration engine, gate rules must be first-class run data.

## Key Decisions

- **Storage:** Add a `gate_rules` column (JSON TEXT) to the `runs` table rather than a separate `gate_rules` table. Gate rules are run-scoped and small — no need for normalization.
- **CLI interface:** `--gates=JSON` for inline, `--gates-file=path` for file-based. Same schema as `SpecGateRule` (check, phase, tier).
- **Evaluation precedence:** Per-run stored rules > agency spec rules > hardcoded defaults. Portfolio/upstream/budget rules are always injected regardless.
- **Default behavior:** Runs without `--gates` use the existing hardcoded `gateRules` map — zero behavioral change for existing users.
- **Schema:** Keyed by transition pair `[from, to]`, matching the existing `gateRules` map structure.

## Open Questions

None — requirements are well-constrained by bead description and existing architecture.
