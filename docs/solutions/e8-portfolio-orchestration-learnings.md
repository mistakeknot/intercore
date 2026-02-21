---
title: "E8 Portfolio Orchestration — Sprint Learnings"
category: architecture
tags: [intercore, portfolio, cross-db, relay, sqlite, gates]
severity: medium
created: 2026-02-21
bead: iv-b1os
---

# E8 Portfolio Orchestration — Sprint Learnings

## What Was Built

Cross-project portfolio orchestration for the intercore kernel: parent/child run hierarchy, project dependency management, portfolio-aware gates, event relay polling loop, and dispatch budget enforcement. Schema v9→v10. ~1200 lines across 17 files.

## Pattern: Cross-DB Polling with Cursor Atomicity

**Problem:** The relay reads phase events from multiple child SQLite databases and writes relay events + cursor updates to the portfolio database. If the relay event is committed but the cursor isn't, events replay on restart.

**Solution:** Wrap both the relay event INSERT and the cursor state REPLACE in a single BEGIN/COMMIT transaction on the portfolio DB. Since both operations target the same database, SQLite's single-writer model guarantees atomicity.

**Key insight:** When aggregating data from multiple SQLite databases into one, always persist the aggregated output and the consumption cursor in the same transaction. This is the standard at-least-once→exactly-once upgrade pattern from message queue design.

## Pattern: Advisory vs. Enforced Limits in CLI Tools

**Problem:** The dispatch limit check reads a relay-maintained counter and then spawns. Concurrent spawns all read the same stale count and all pass.

**Learning:** For CLI-level resource limits, choose one:
1. **Advisory** (current): document it's best-effort, log warnings, accept overruns. Good enough for most cases.
2. **Enforced**: use an atomic UPDATE with rows-affected check (`SET count = count + 1 WHERE count < max`). Requires the counter to live in the same DB as the spawn record.

Don't claim enforcement when the mechanism is advisory. The relay-maintained cache model is inherently TOCTOU-vulnerable. We chose advisory with documentation.

## Mistake: Sentinel Values for Type Discrimination

**What happened:** Portfolio runs were identified by `project_dir = ""` — an empty string sentinel. Four review agents independently flagged this as fragile. Any run accidentally created with empty project_dir becomes an accidental portfolio.

**Better approach:** An explicit `is_portfolio` column (or a `run_type` enum) makes the discrimination schema-enforced rather than convention-enforced. We kept the sentinel for now (schema change cost > risk at current scale) but should add the column in a future hardening pass.

## Mistake: time.After in Polling Loops

**What happened:** `time.After(interval)` inside a `for` loop creates one timer per iteration that GC must collect. Replace with `time.NewTicker` + `defer ticker.Stop()`.

**Rule:** Never use `time.After` in long-lived loops. Always use `time.NewTicker` or `time.NewTimer` with explicit `.Stop()`.

## Mistake: EventCancel Mapped to EventChildCompleted

**What happened:** A cancelled child emitted `child_completed`, which could cause the portfolio gate to treat cancellation as successful completion.

**Rule:** When mapping event types across boundaries, map each source event explicitly. Don't collapse semantically different events (cancel vs. complete) into one category.

## Decision Validated: Interface Injection for Gate Extension

The `PortfolioQuerier` interface added to the gate evaluation signature was clean — it allowed the gate to query children without importing the store package, and nil-checks gracefully degrade for non-portfolio runs. All four reviewers approved this pattern.

## Decision Validated: Read-Only DB Pool for Child Access

Opening child databases with `?mode=ro` and caching handles in a mutex-protected pool was validated by all reviewers. The one caveat: add cache invalidation when a child DB is rotated/migrated (not urgent, documented for future).

## Migration Pattern: Wide Guards with Duplicate-Column Safety

The v9→v10 migration guard `currentVersion >= 3 && currentVersion < 10` correctly fires for all DBs that have the `runs` table. The `isDuplicateColumnError` guard catches re-runs. However, the v5→v6 migration block is missing its upper bound (`< 6`). All migration blocks should have both lower and upper bounds for clarity.

## Complexity Calibration

Estimated C4, actual was C4. The cross-DB transaction atomicity issue (relay cursor) was the trickiest finding — it's not immediately obvious from reading the code but becomes a real problem on any process restart. The four-agent review caught it within minutes. Multi-agent quality gates continue to pay for themselves on C3+ work.
