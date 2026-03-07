# AGENTS.md — Intercore

Kernel layer (Layer 1) of the Demarch autonomous software agency platform. Host-agnostic Go CLI (`ic`) backed by a single SQLite WAL database providing the durable system of record for runs, phases, gates, dispatches, events, token budgets, coordination locks, discovery pipelines, work lanes, and scheduling.

## Canonical References
1. [`PHILOSOPHY.md`](../../PHILOSOPHY.md) — direction for ideation and planning decisions.
2. `CLAUDE.md` — implementation details, architecture, testing, and release workflow.

## Quick Reference

```bash
go build -o ic ./cmd/ic                    # Build
go test ./...                              # Unit tests
bash test-integration.sh                   # Integration tests
ic init                                    # Create/migrate DB
ic health                                  # Check DB + schema
ic version                                 # CLI + schema versions
```

**Module:** `github.com/mistakeknot/intercore`
**Location:** `core/intercore/`
**Database:** `.clavain/intercore.db` (project-relative, auto-discovered by walking up from CWD)
**Schema:** v20 (24 tables, `PRAGMA user_version` tracked)
**CLI version:** 0.3.0

## Topic Guides

| Topic | File | Covers |
|-------|------|--------|
| CLI Reference | [agents/cli-reference.md](agents/cli-reference.md) | All `ic` commands, flags, exit codes, publish pipeline |
| Modules | [agents/modules.md](agents/modules.md) | Dispatch, Phase, Gate, Event, Coordination, Scheduler, Lane, Discovery, Cost, Portfolio, Lock, supporting libraries |
| Architecture | [agents/architecture.md](agents/architecture.md) | Security model, SQLite patterns, schema upgrade |
| Bash Wrappers | [agents/bash-wrappers.md](agents/bash-wrappers.md) | lib-intercore.sh (45 functions) |
| Testing & Recovery | [agents/testing.md](agents/testing.md) | Test suites, DB corruption, stuck locks, schema mismatch |

## Philosophy Alignment Protocol
Review [`PHILOSOPHY.md`](../../PHILOSOPHY.md) during:
- Intake/scoping
- Brainstorming
- Planning
- Execution kickoff
- Review/gates
- Handoff/retrospective

For brainstorming/planning outputs, add two short lines:
- **Alignment:** one sentence on how the proposal supports the module's purpose within Demarch's philosophy.
- **Conflict/Risk:** one sentence on any tension with philosophy (or 'none').

If a high-value change conflicts with philosophy, either:
- adjust the plan to align, or
- create follow-up work to update `PHILOSOPHY.md` explicitly.

## Decay Policy

Operational state (C1) follows intermem's decay model adapted for kernel data:

| Data type | Grace period | TTL | Hysteresis | Action |
|-----------|-------------|-----|------------|--------|
| Completed runs | 30 days | 30d from completion | N/A | Pruned from active queries (retained in DB for audit) |
| Coordination locks | Per-lock TTL | Lock-specific (default 60s) | N/A | Auto-released at expiry |
| Dispatch records | 30 days | 30d from completion | N/A | Excluded from cost aggregation |
| Event stream | 90 days | 90d retention | N/A | Old events excluded from reactor processing |

**Standard pattern:** Grace period → TTL expiry → no hysteresis (kernel state is operational, not knowledge). Intercore uses TTL-based cleanup rather than confidence decay because C1 data has a clear "done" state — completed runs don't gradually lose relevance, they become irrelevant after their monitoring window closes. Sentinel auto-prune runs synchronously in the same transaction as new writes.
