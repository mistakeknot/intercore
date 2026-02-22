---
title: C1 Agency Specs Learnings
category: architecture
severity: medium
bead: iv-afwv
date: 2026-02-21
tags: [agency, gate-evaluation, yaml, json, validation, cli, intercore]
---

# C1: Agency Specs Learnings

Sprint `iv-afwv` — declarative per-stage agency configuration with YAML specs, Go parser/validator, CLI, and integration with gates and model routing.

## What Was Built

- **Type system** (`internal/agency/agency.go`) — Spec, Meta, AgentEntry, ModelConfig, GateConfig, BudgetConfig, CapabilitySet with dual `yaml`/`json` tags
- **Parser** (`parser.go`) — `ParseFile`/`ParseBytes` using gopkg.in/yaml.v3
- **Validator** (`validate.go`) — structural + semantic validation against known kernel phases and check types
- **CLI** (`cmd/ic/agency.go`) — load, validate, show, capabilities subcommands
- **5 default specs** in `hub/clavain/config/agency/` — discover, design, build, ship, reflect
- **Gate integration** — `SpecRules` field on `GateConfig` for spec-defined gate rules with per-rule tier overrides
- **Model routing integration** — kernel state store overrides in `lib-routing.sh`
- **Sprint creation integration** — `sprint_create()` auto-loads agency specs

## Patterns Discovered

### 1. Dual YAML/JSON Tags Are Mandatory for Go↔Shell Interop

**Problem:** Go structs parsed from YAML get stored as JSON in the state store (`state.Set`). Bash/jq consumers read the JSON with lowercase keys. Without explicit `json` tags, `json.Marshal` produces capitalized field names (`Default` vs `default`), breaking all jq queries downstream.

**Fix:** Every struct field gets both tags: `yaml:"default" json:"default"`. This was caught in plan review (P0 finding) before any code was written.

**Lesson:** Any time Go types cross the Go→JSON→jq boundary, require dual tags. Add this as a validation check in code review.

### 2. Cross-Stage Gate Phase References Require Full Phase Set

**Problem:** Gate entry conditions can reference phases from other stages (e.g., design's entry gate checks for brainstorm artifacts from discover stage). Initial implementation validated gate phases against only the spec's own `meta.phases`, rejecting valid cross-stage references.

**Fix:** `validateGateRule` receives `phaseSet` (all known kernel phases), not `specPhases`. The parameter name and error message must match — "not a known kernel phase" not "not in meta.phases".

**Lesson:** When a validation boundary seems obviously correct, check if the domain has a cross-boundary case. Gate rules are inherently cross-stage; restricting them to the local spec breaks the design intent.

### 3. Per-Rule Tier Override Needs Explicit Propagation

**Problem:** The `SpecGateRule` type has a `Tier` field for per-rule hardness overrides, but the conversion from SpecGateRule to internal `gateRule` initially dropped it. The overall gate tier was derived solely from `cfg.Priority`, making the per-rule tier structurally dead.

**Fix:** (a) Pass `tier` in the conversion: `gateRule{..., tier: sr.Tier}`. (b) After building the rules list, scan for tier overrides and escalate the overall tier if any rule is stricter (hard > soft > default).

**Lesson:** When adding a field to a type, grep for all sites that construct that type. A new field that's never populated is a latent correctness bug.

### 4. AddBatch vs Add: Key Semantics Matter

**Problem:** `action.Store.AddBatch` uses the map key as the phase name. The loader initially used `phase:command` composite keys, causing wrong phase names in the database.

**Fix:** Switched to individual `action.Store.Add()` calls per agent entry, using `errors.Is(err, action.ErrDuplicate)` for idempotent reload.

**Lesson:** Always verify the contract of batch APIs, especially key-is-value patterns. Individual calls with explicit error handling are safer when the batch semantics don't align.

### 5. State Store as Extension Point Avoids Schema Migrations

**Design choice:** Instead of adding new tables for model overrides, gate rules, and capabilities, we used the existing `state` table (key-value with JSON payload and scope_id). This means agency specs don't require a schema migration — they work with schema v14.

**Tradeoff:** No foreign keys or indexing on the structured data inside the JSON. Acceptable because lookups are by exact key+scope and data volume is tiny (5 stages × a few keys per run).

### 6. errors.Is Over strings.Contains for Sentinel Errors

**Problem:** Duplicate detection in the loader used `strings.Contains(err.Error(), "duplicate")` — fragile if the error message text changes or another layer wraps a different "duplicate" error.

**Fix:** Use `errors.Is(aerr, action.ErrDuplicate)` — matches the established pattern in `cmd/ic/action.go` and works correctly through error wrapping.

### 7. Exit Code Conventions Need Documentation

The codebase uses a 3-tier exit code scheme: 3=usage, 2=infra, 1=app-logic. This wasn't documented and the new code defaulted everything to 1. After review, `openDB` failures were corrected to exit 2.

**Lesson:** Exit code conventions should be in AGENTS.md or a constants file, not just established by convention in existing code.

## Complexity Calibration

Estimated C3 (moderate), actual was C3. The 10-task plan was appropriate. The plan review step (flux-drive before execution) caught 3 P0 issues that would have been expensive to fix after implementation. The quality gates step found 6 more issues (2 P0, 3 P1, 1 P2) in the implementation.

## What Worked Well

- **Plan review before execution** — caught yaml.v3 missing, json tag absence, and gate import-cycle risk before any code was written
- **Individual task execution** — 10 bite-sized tasks kept each step testable
- **Integration tests** — 12 assertions caught the AddBatch key mismatch immediately
- **Dual reviewer agents** (correctness + quality) — complementary findings with no overlap
