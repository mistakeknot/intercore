# Interspect — Implementation Roadmap

**Version:** 2.0 (post-Oracle review)
**Date:** 2026-02-15
**PRD:** `docs/product/interspect-prd.md`

---

## Overview

Three phases, observability-first. Each phase validates assumptions before expanding scope. Autonomy is earned through data, not assumed.

```
Phase 1              Phase 2                Phase 3
Evidence +           Overlays +             Autonomy +
Reporting            Canary Alerting        Shadow Testing
[4 weeks]            [4 weeks]              [6 weeks]
```

**Key principle (from Oracle review):** Ship the audit/debug UX first, then overlays as safe experiments, then autonomy. The real moat is evals + observability + safe change control, not autonomous modification.

---

## Phase 1: Evidence + Reporting (4 weeks)

**Goal:** Validate which evidence signals are useful. Zero modifications applied.
**Gate:** >=50 evidence events across >=10 sessions before Phase 2.

### Week 1-2: Foundation

| Task | Description |
|------|-------------|
| SQLite schema + init | Create `.clavain/interspect/interspect.db` with evidence, sessions, canary, modifications tables. WAL mode. Init script idempotent. |
| Evidence collection hook | PostToolUse hook for `/resolve` dismissals. Writes to SQLite via `sqlite3` CLI. |
| Session lifecycle hooks | Extend session-start.sh to write session start event. Add session-end event to Stop hook. Non-blocking (not in critical path). |
| `/interspect:correction` command | Explicit signal command: `<agent> <description>`. Writes `agent_wrong` event to evidence. Sole mechanism for human override signals. |

### Week 3-4: Reporting

| Task | Description |
|------|-------------|
| `/interspect` command | Pattern detection: query evidence store, group by agent + event type, compute frequencies. Show suggested tunings with counting-rule thresholds. |
| `/interspect:status` command | Show modification history, canary state, metrics vs baseline for a component. |
| `/interspect:evidence` command | Human-readable evidence summary for an agent. Query + format. |
| `/interspect:health` command | Signal status dashboard: which collection points are active, evidence counts, staleness. |
| Session-start summary | Inject "Interspect: N overlays active, M canaries monitoring" via SessionStart hook. Non-blocking. |
| Evidence sanitization | Strip control chars, truncate to 500 chars, reject injection patterns, tag with hook_id for provenance. |

### Phase 1 Deliverables
- Working evidence store collecting real data
- 4 reporting commands functional — user can answer "what changed and why" in 10 seconds
- Session-start summary when overlays/canaries exist
- Validated: which signals produce useful data, which don't

---

## Phase 2: Overlays + Canary Alerting (4 weeks)

**Goal:** Apply safe, reversible overlays for human approval. Canary monitors and alerts (not auto-reverts).
**Gate:** Propose-mode acceptance rate >=70% across >=10 proposals before Phase 3.

### Week 5-6: Overlay System

| Task | Description |
|------|-------------|
| Counting-rule confidence gate | Simple threshold: >=3 sessions AND >=2 projects AND >=N events of same typed pattern. No weighted formula — calibrate from real data later. |
| Protected paths manifest | Create `.clavain/interspect/protected-paths.json` with allow-list and protected list. Mechanical enforcement. |
| Git pre-commit hook | Reject `[interspect]` commits touching protected paths. |
| Overlay system (Type 1) | Feature-flag prompt overlays: `.clavain/interspect/overlays/<agent>/<overlay-id>.md`. Runtime concatenation: base prompt + active overlays. Rollback = disable overlay. 500-token budget per overlay. |
| Routing overrides (Type 2) | Per-project `routing-overrides.json` for agent exclusions. Enable/disable artifacts, not prompt edits. |
| Git operation serialization | `flock` wrapper for all git add/commit operations. Concurrent session safety. |
| Secret detection | Grep for credential patterns in evidence before insertion. Redact matches. |

### Week 7-8: Canary + UX

| Task | Description |
|------|-------------|
| Canary monitoring (detect + alert) | SQLite canary records. 20-use/14-day windows. Three metrics: override rate, FP rate, finding density. Rolling baseline. **Alert on degradation, do not auto-revert.** |
| `/interspect:revert` command | One-command revert of overlay/routing change + blacklist pattern. Manual trigger only. |
| Structured commit message format | `[interspect]` commits with type, target, evidence summary, confidence. Parseable for audit. |
| Statusline integration | Interline: `[inspect:canary(fd-safety)]` showing active canary count and any degradation alerts. |
| Global rate limiter | Max 5 modifications per 10 sessions. System-wide circuit breaker. |

### Phase 2 Deliverables
- Overlay system (Type 1) and routing overrides (Type 2) in propose mode
- Canary monitoring with 3 metrics + Galiana recall cross-check — **alerts, not auto-reverts**
- Protected paths enforced mechanically
- One-command revert with blacklisting
- Secret detection in evidence pipeline
- No direct edits to canonical agent prompts

---

## Phase 3: Autonomy + Eval Corpus (6 weeks)

**Goal:** Enable opt-in autonomous overlays for Types 1-2. Add prompt tuning (Type 3) only after eval corpus exists.
**Gate:** Canary alert rate <10% across >=20 canary windows AND eval corpus covers >=3 agent domains.

### Week 9-11: Autonomy + Evals

| Task | Description |
|------|-------------|
| Autonomous mode flag | `/interspect:enable-autonomy` and `:disable-autonomy`. Flag in protected manifest. Low/medium-risk overlays auto-apply with canary. |
| Eval corpus construction | Build eval suite from production reviews — real failure modes, not synthetic tests. **Hard prerequisite for Type 3.** No eval corpus = no prompt tuning. |
| Counterfactual shadow evaluation | Run candidate overlay/routing in shadow path on real invocations. Log "would-have-changed" deltas. Changes must win in shadow eval before auto-applying. |
| Prompt tuning (Type 3) in propose mode | Overlay-based: `.clavain/interspect/overlays/<agent>/` (not direct `.md` edits). Galiana recall cross-check required. Eval corpus required. Rare agents = propose-only permanently. |

### Week 12-14: Safety Hardening + Meta-Learning

| Task | Description |
|------|-------------|
| Privilege separation | Split into unprivileged proposer (writes to staging dir only) + privileged applier (allowlisted patch format). Proposer cannot write to repo. |
| Meta-learning loop | Modification outcomes as evidence. Root-cause taxonomy. Successful modifications lower future risk; reverted ones raise it. |
| Circuit breaker | 3 reverts in 30 days disables target. Files beads issue. `/interspect:unblock` to re-enable. |
| Conflict detection | Same target modified-then-reverted by different evidence patterns -> escalate to human. |
| `/interspect:reset` command | Revert all active overlays, archive evidence. Nuclear option with confirmation. |
| `/interspect:disable` command | Pause all interspect activity. Hooks check disable flag and no-op. |

### Phase 3 Deliverables
- Autonomous mode for Types 1-2 (overlays + routing) with counterfactual shadow evaluation
- Prompt tuning (Type 3) in propose mode — only with eval corpus
- Privilege separation between proposer and applier
- Meta-learning with root-cause attribution
- Full command suite operational

---

## Phase 4: Evaluate + Expand (Ongoing)

**Goal:** Recalibrate based on real data. Decide v2 scope.

| Activity | Timeline |
|----------|----------|
| Calibrate counting-rule thresholds against 3 months of data | Month 4 |
| Evaluate whether weighted confidence function adds value over counting rules | Month 4 |
| Cross-model shadow testing (Oracle as independent judge) | Month 4 |
| Consider autonomous prompt tuning if propose acceptance >80% AND eval corpus sufficient | Month 5 |
| Evaluate Types 4-6 need based on manual improvement patterns | Month 5 |
| Annual threat model review | Month 12 |

---

## Design Revisions Required Before Phase 1

These must be addressed in the design doc before implementation begins:

| # | Issue | Action |
|---|-------|--------|
| 1 | AskUserQuestion response capture infeasible | Update design SS3.1.4: remove claim, confirm `/interspect:correction` as sole override signal |
| 2 | Tier 1 session-scoped mods cut (Oracle rec) | Remove Tier 1 from design. Log patterns but don't auto-adjust mid-session |
| 3 | Sidecar injection -> overlay system | Replace sidecar mechanism with overlay file system: `.clavain/interspect/overlays/<agent>/` with runtime concatenation |
| 4 | Success metrics absent | Add SS5.5 with the 5 metrics from PRD SS6 |
| 5 | Flux-drive integration contract | Update interflux to document overlay consumption and routing-overrides.json |
| 6 | Git operation serialization missing | Add flock wrapper for git operations |
| 7 | Add privilege separation design | New subsection: proposer/applier split with allowlisted patch format |
| 8 | Confidence function -> counting rules | Replace weighted formula with simple thresholds (>=3 sessions, >=2 projects, >=N events) |

---

## Scope Changes from Oracle Review

| Change | Rationale | Impact |
|--------|-----------|--------|
| **Drop Tier 1** (session-scoped mods) | Complexity for marginal gain; double-counting risk; "why did behavior change mid-session?" confusion | Cut the Tier 1 work item; simplify design |
| **Overlay system** replaces direct prompt editing | Instant rollback (disable overlay), A/B testable, upstream-mergeable, no long-lived prompt forks | Restructure Type 1 and Type 3 implementation |
| **Counting rules** replace weighted confidence | Easier to reason about, debug, and explain; weighted formula is arbitrary until calibrated | Simplify the counting-rule gate |
| **Canary: detect+alert** not auto-revert | 80% of safety value with 20% of complexity; avoids revert-chain edge cases and "why did it change back?" | Simplify the canary task, cut canary verdict engine complexity |
| **Eval corpus as hard prerequisite** for Type 3 | Synthetic tests give false confidence; no eval = no prompt tuning | Eval corpus construction becomes a task; it gates prompt tuning |
| **Privilege separation** added | Proposer can't write to repo; applier enforces allowlist. Addresses "unenforceable meta-rules" critique | New bead for Phase 3 |
| **Counterfactual shadow evaluation** added | Shadow-traffic before real changes; builds paired comparison dataset; no autonomy risk | New bead for Phase 3 |

---

## Dependency Graph (Critical Path)

```
Design Fixes (prereq)
  |
  v
Phase 1: Schema ──> Evidence Hook ──> Commands ──> Session Summary
                ──> Session Hooks     ──> Sanitization
                ──> Correction Cmd
  |
  v (gate: >=50 events, >=10 sessions)
Phase 2: Protected Paths ──> Counting Rules ──> Overlay System ──> Canary (alert)
         Pre-commit Hook     Git Flock         Routing Overrides   Revert Cmd
         Secret Detection                                          Rate Limiter
  |
  v (gate: acceptance >=70%, >=10 proposals)
Phase 3: Eval Corpus ──> Shadow Eval ──> Prompt Tuning (overlay-based)
         Autonomy Flag    Privilege Sep    Meta-Learning
                                          Circuit Breaker
```

---

## Resource Estimate

| Phase | Effort | Calendar |
|-------|--------|----------|
| Design fixes | 2-3 sessions | 1-2 days |
| Phase 1 | 8-12 sessions | 4 weeks |
| Phase 2 | 8-12 sessions | 4 weeks |
| Phase 3 | 10-14 sessions | 6 weeks |
| **Total to autonomous** | **~30 sessions** | **~14 weeks** |

Phase 2 is lighter than before (no auto-revert complexity, no Tier 1, simpler confidence). Phase 3 is slightly heavier (eval corpus, privilege separation) but more safely scoped.
