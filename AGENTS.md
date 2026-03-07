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
