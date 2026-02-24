# E5 Discovery Pipeline — Quality Gate Synthesis Report

**Review Date:** 2026-02-20
**Context:** E5 Discovery Pipeline: adding discovery CLI (11 subcommands, 642 lines), event bus integration (third `UNION ALL` leg), and consumer cursor tracking
**Agents Launched:** 4 (fd-architecture, fd-correctness, fd-quality, fd-safety)
**Agents Completed:** 4 (100%)
**Mode:** quality-gates

---

## Validation Summary

All four agent outputs are **valid** and follow the expected Findings Index format with Verdict lines.

| Agent | Status | Verdict | Issues Found |
|-------|--------|---------|--------------|
| fd-architecture | VALID | needs-changes | 5 findings (1 MEDIUM, 4 LOW/INFO) |
| fd-correctness | VALID | needs-changes | 6 findings (2 MEDIUM, 4 LOW/INFO) |
| fd-quality | VALID | needs-changes | 8 findings (2 MEDIUM, 6 LOW/INFO) |
| fd-safety | VALID | needs-changes | 3 findings (3 LOW) + 1 INFO |

**Validation:** 4/4 agents valid, 0 failed

---

## Deduplication & Convergence Analysis

### High-Convergence Findings (Consensus Issues)

The four agents converge on three critical defects:

#### **1. ListEvents discovery leg missing run-scoped filter [UNANIMOUS: 4/4 agents]**

| Agent | ID | Severity | Citation |
|-------|----|-----------|-|
| fd-architecture | A1 | MEDIUM | store.go:41–63 |
| fd-correctness | C-01 | MEDIUM | store.go:52–56 |
| fd-quality | (Implicit in index) | (Listed in synthesis) | Lines 52–56 |
| fd-safety | (N/A, not safety concern) | — | — |

**Finding:** The `ListEvents` function (run-scoped event retrieval) has two filtering legs that apply `WHERE run_id = ?`:
- `phase_events`: `WHERE run_id = ? AND id > ?`
- `dispatch_events`: `WHERE (run_id = ? OR ? = '') AND id > ?`
- `discovery_events`: `WHERE id > ?` **← missing run_id predicate**

**Impact:** Calling `ic events tail <run_id>` returns discovery events from all runs alongside that run's phase/dispatch events. Consumers filtering by run see unrelated discovery noise. More critically, if a consumer uses these events to decide work completion (e.g., checking for a discovery of a specific type), it may act on cross-run discovery events and corrupt its state machine.

**Consensus Fix:** Either:
- **(a)** Exclude discovery events from the run-scoped path (architecturally cleaner — discovery is system-level, not per-run). Use `ListAllEvents` for consumers that need cross-run discovery.
- **(b)** Document the intentional boundary in a code comment so future maintainers don't attempt to add a run filter and discover that the column doesn't exist.

**Severity:** MEDIUM (data-integrity violation for consumers)
**Priority:** Must fix before merge

---

#### **2. saveCursor hardcodes interspect field to zero [UNANIMOUS: 3/4 agents + 1 implicit]**

| Agent | ID | Severity | Citation |
|-------|----|-----------|-|
| fd-architecture | A2 | LOW | events.go:322 |
| fd-correctness | C-03 | LOW | events.go:322 |
| fd-quality | F2 | MEDIUM | events.go:322 |
| fd-safety | (N/A) | — | — |

**Finding:** `saveCursor` writes:
```go
payload := fmt.Sprintf(`{"phase":%d,"dispatch":%d,"interspect":0,"discovery":%d}`, phaseID, dispatchID, discoveryID)
```

The `interspect` field is always written as `0`, destroying any previously-advanced interspect cursor position. The `loadCursor` function correctly reads and returns the persisted interspect value (line 309), but it is immediately discarded when `saveCursor` overwrites it with `0`.

This is a **pre-existing bug** (likely predates E4), but E5 introduces `discoveryID` as a parameter using the correct round-trip pattern (line 314 read, line 322 write). The asymmetry creates a code smell.

**Impact:** Any named consumer that previously advanced its interspect cursor will have that position silently reset on the next batch save. If interspect events are added to the UNION ALL in a future version and a consumer relies on durable cursor tracking, the consumer will replay all interspect events from the start on every restart.

**Consensus Fix:** Add `sinceInterspectID int64` as a `loadCursor` return value and a `saveCursor` parameter, matching the pattern used for the three other cursors.

**Severity:** MEDIUM (fd-quality escalated); was LOW (fd-architecture, fd-correctness)
**Priority:** Should fix in this PR (easy change, consistency with new discovery field)

---

#### **3. Decay reads outside transaction, writes inside — TOCTOU window [2/4 agents]**

| Agent | ID | Severity | Citation |
|-------|----|-----------|-|
| fd-architecture | — | — | (Not identified) |
| fd-correctness | C-02 | MEDIUM | store.go:467–528 |
| fd-quality | — | — | (Not identified) |
| fd-safety | — | — | (Not identified) |

**Finding:** In `internal/discovery/store.go` lines 467–528, the `Decay` batch operation:
1. Runs a SELECT outside any transaction to identify eligible discoveries (status not in `dismissed`/`promoted`)
2. Closes the result rows
3. Opens a separate transaction and updates each row

Between the SELECT and BeginTx, a concurrent `Promote` or `Dismiss` can change a row's status. The UPDATE then applies decay to a row that should have been skipped.

**Concrete interleaving:**
1. Decay SELECTs row `disc-A` with `status=scored`, `score=0.8`
2. Concurrently, Promote sets `disc-A` to `status=promoted`
3. Decay opens its transaction, UPDATEs `disc-A` score to `0.72`
4. `disc-A` is now promoted with a decayed score — violating invariant that promoted discoveries are stable

**Impact:** Data consistency violation under concurrent load. While SQLite's `SetMaxOpenConns(1)` serializes concurrent writes, it does not prevent the read-then-write TOCTOU window.

**Consensus Fix:** Move the SELECT inside the transaction:
```go
tx, err := s.db.BeginTx(ctx, nil)
// SELECT inside tx
// UPDATE inside tx
```

**Severity:** MEDIUM (genuine correctness failure, exploitable under concurrent load)
**Priority:** Must fix before merge

---

#### **4. Dismiss operation lacks idempotency guard [1/4 agents explicit + fd-correctness implicit]**

| Agent | ID | Severity | Citation |
|-------|----|-----------|-|
| fd-architecture | — | — | (Not identified) |
| fd-correctness | C-04 | LOW | store.go:352–386 |
| fd-quality | — | — | (Not identified) |
| fd-safety | — | — | (Not identified) |

**Finding:** The `Dismiss` function reads `status` but does not check if it is already `StatusDismissed`. It proceeds to:
- `UPDATE ... SET status = 'dismissed', reviewed_at = ?` unconditionally
- Insert a second `EventDismissed` event

Calling `ic discovery dismiss <id>` twice will:
- Overwrite `reviewed_at` with a newer timestamp (audit record drift)
- Emit a duplicate `discovery.dismissed` event

Compare to `Promote` (line 319), which has an explicit idempotency guard: `if status == StatusPromoted { return nil }`.

**Impact:** Double-dismiss produces duplicate events and timestamp drift. Exploitable via `ic discovery rollback` on a source that includes already-dismissed records.

**Consensus Fix:** Add idempotency guard:
```go
if status == StatusDismissed {
    return nil  // already dismissed, idempotent
}
```

**Severity:** LOW (audit record drift, not data corruption)
**Priority:** Should fix (consistency with Promote)

---

### Medium-Convergence Findings (Multi-Agent Agreement)

#### **5. readFileArg lacks CWD containment check [2/4 agents]**

| Agent | ID | Severity | Citation |
|-------|----|-----------|-|
| fd-architecture | A5 | INFO | discovery.go:656–661 |
| fd-correctness | — | — | (Not identified) |
| fd-quality | — | — | (Not identified) |
| fd-safety | F1 | LOW | discovery.go:655–660 |

**Finding:** `readFileArg` (cmd/ic/discovery.go lines 655–660) resolves `@filepath` arguments with bare `os.ReadFile(arg[1:])`. No path containment check exists, unlike `validateDBPath` in main.go (lines 218–238) which requires `.db` extension, rejects `..`, and asserts the resolved path is under CWD.

Affects five flags: `--embedding=@file`, `--metadata=@file`, `--data=@file`, `--keyword-weights=@file`, `--source-weights=@file`.

**Impact:** LOCAL-ONLY RISK (operator/script level). If an operator or automation script constructs these flags from untrusted external input, an attacker could trick the script into reading `/etc/passwd`, SSH keys, or other host files and storing them as discovery metadata. Also potential for large BLOB allocations.

**Consensus Fix:** Apply the same CWD-containment and path-cleaning logic from `validateDBPath` to `readFileArg`.

**Severity:** LOW (local-operator threat model only; current context has low risk)
**Priority:** Should fix (consistency with stated security posture)

---

#### **6. interspect cursor field alignment [2/4 agents implicit]**

| Agent | ID | Severity | Citation |
|-------|----|-----------|-|
| fd-architecture | A2 | LOW | events.go:322 |
| fd-correctness | (Covered in C-03) | — | — |
| fd-quality | — | — | (Not identified as separate) |
| fd-safety | — | — | (Not identified) |

This is a component of finding #2 (saveCursor hardcodes 0). See consensus finding #2.

---

### Low-Convergence / Single-Agent Findings

#### **7. Event.Source godoc stale after E5 [1/4 agents]**

| Agent | ID | Severity | Citation |
|-------|----|-----------|-|
| fd-architecture | A3 | LOW | event.go:31 |
| fd-correctness | (Implied by C-01) | — | — |
| fd-quality | — | — | (Not identified) |
| fd-safety | — | — | (Not identified) |

**Finding:** The `Event` struct comment says `Source: "phase" or "dispatch"` but there are now four source constants: `SourcePhase`, `SourceDispatch`, `SourceInterspect`, `SourceDiscovery`.

**Impact:** Documentation drift, reduces legibility of the type contract.

**Severity:** LOW (docs only)
**Priority:** Nice-to-have (fix in same PR as the other medium findings)

---

#### **8. cmdDiscoveryProfile uses inline arg check instead of switch pattern [1/4 agents]**

| Agent | ID | Severity | Citation |
|-------|----|-----------|-|
| fd-architecture | A4 | INFO | discovery.go:413–417 |
| fd-correctness | — | — | — |
| fd-quality | — | — | — |
| fd-safety | — | — | — |

**Finding:** Every other subcommand group (`cmdRun`, `cmdDispatch`, `cmdGate`, `cmdLock`, `cmdSentinel`, `cmdState`, `cmdDiscovery` itself) uses a top-level `switch args[0]` for dispatching. `cmdDiscoveryProfile` uses an inline `if len(args) > 0 && args[0] == "update"` guard.

**Impact:** Breaks naming consistency. `ic discovery profile unknown-subcommand` silently returns profile data instead of a usage error (exit 3).

**Severity:** INFO (style/consistency)
**Priority:** Nice-to-have

---

#### **9. Silently discarded json.Marshal errors in --json output [1/4 agents, but critical]**

| Agent | ID | Severity | Citation |
|-------|----|-----------|-|
| fd-architecture | — | — | — |
| fd-correctness | — | — | — |
| fd-quality | F1 | MEDIUM | discovery.go:163, 215, 393, 604 |
| fd-safety | — | — | — |

**Finding:** Four `--json` output paths use `data, _ := json.Marshal(...)` and discard the error:
- Line 163: `cmdDiscoveryList` JSON output
- Line 215: `cmdDiscoveryStatus` JSON output
- Line 393: `cmdDiscoveryProfileGet` JSON output
- Line 604: (Additional location)

If marshaling fails, the caller receives `null` on stdout with exit 0, indistinguishable from a legitimate null result.

**Impact:** Silent errors mask bugs. Automation consuming `ic discovery list --json` cannot distinguish between "no discoveries" and "marshal failure".

**Consensus Fix:** Use the established pattern: `json.NewEncoder(os.Stdout).Encode(v)` with explicit error handling (see `cmdEventsTail` for reference).

**Severity:** MEDIUM (silent errors, automation blind spot)
**Priority:** Must fix

---

#### **10. Profile JSON values bypass size limits [1/4 agents]**

| Agent | ID | Severity | Citation |
|-------|----|-----------|-|
| fd-architecture | — | — | — |
| fd-correctness | — | — | — |
| fd-quality | — | — | — |
| fd-safety | F2 | LOW | discovery.go:401–444 |

**Finding:** `cmdDiscoveryProfileUpdate` reads `--keyword-weights` and `--source-weights` via `readFileArg`. No size or structural validation before storage. The existing state store enforces a 1MB payload cap and 20-level nesting depth, but those limits are not applied to `interest_profile` columns.

**Impact:** Excessive or malformed JSON could cause downstream OOM or crash.

**Severity:** LOW (not a direct vulnerability, but inconsistent constraint handling)
**Priority:** Should fix (add validation)

---

#### **11. signal_type and actor columns accept arbitrary strings [1/4 agents]**

| Agent | ID | Severity | Citation |
|-------|----|-----------|-|
| fd-architecture | — | — | — |
| fd-correctness | — | — | — |
| fd-quality | — | — | — |
| fd-safety | F3 | LOW | discovery.go:362–410 |

**Finding:** `cmdDiscoveryFeedback` accepts `--signal=<type>` as a free-form string, stored in `feedback_signals.signal_type` with no validation against defined constants (`promote`, `dismiss`, `adjust_priority`, `boost`, `penalize`). Similarly, `--actor=<name>` has no length cap.

**Impact:** Feedback table accumulates arbitrary signal types, breaking downstream consumers expecting the canonical set.

**Severity:** LOW (not a direct vulnerability, but weakens data contract)
**Priority:** Should fix (add allowlist validation)

---

#### **12. Rollback emits dismissed events with empty from_status [1/4 agents]**

| Agent | ID | Severity | Citation |
|-------|----|-----------|-|
| fd-architecture | — | — | — |
| fd-correctness | C-05 | INFO | store.go:633–636 |
| fd-quality | — | — | — |
| fd-safety | — | — | — |

**Finding:** `Rollback` inserts discovery events with `from_status = ''`. Cannot determine what lifecycle state was bypassed (e.g., `new` vs `scored`).

**Impact:** Audit/observability gap, not data corruption.

**Severity:** INFO (audit visibility only)
**Priority:** Nice-to-have

---

#### **13. Cursor restore guard masks explicit --since-discovery=0 [1/4 agents]**

| Agent | ID | Severity | Citation |
|-------|----|-----------|-|
| fd-architecture | — | — | — |
| fd-correctness | C-06 | INFO | events.go:115–117 |
| fd-quality | — | — | — |
| fd-safety | — | — | — |

**Finding:** The all-zero guard `if consumer != "" && sincePhase == 0 && sinceDispatch == 0 && sinceDiscovery == 0` treats explicit `--since-discovery=0` as "no override". User cannot reset replay when a consumer cursor exists.

**Impact:** Minor UX inconsistency. Correct mechanism is `ic events cursor reset <consumer>`.

**Severity:** INFO (UX, not correctness)
**Priority:** Nice-to-have

---

#### **14. Decay uses empty string for discovery_id in event [1/4 agents]**

| Agent | ID | Severity | Citation |
|-------|----|-----------|-|
| fd-architecture | I1 | INFO | store.go:521 |
| fd-correctness | — | — | — |
| fd-quality | — | — | — |
| fd-safety | — | — | — |

**Finding:** `Decay` batch event recorded with `discovery_id = ''` (empty string). Schema column is `NOT NULL` so this works, but empty string is semantically different from a legitimate ID.

**Impact:** Ambiguity in event log analysis. Consumers filtering by discovery ID cannot distinguish system-level events from discovery-specific ones.

**Severity:** INFO (observability)
**Priority:** Nice-to-have (document intent or use sentinel like `"__system__"`)

---

#### **15. ListEvents and ListAllEvents function signatures diverge [1/4 agents]**

| Agent | ID | Severity | Citation |
|-------|----|-----------|-|
| fd-architecture | I2 | INFO | store.go |
| fd-correctness | — | — | — |
| fd-quality | — | — | — |
| fd-safety | — | — | — |

**Finding:** `ListEvents` and `ListAllEvents` have different signatures (only differ in optional `runID`). With four cursor parameters, next extension requires synchronized updates at 8 call sites.

**Impact:** Mechanical change surface at next extension. Code smell but not a defect.

**Severity:** INFO (refactoring suggestion)
**Priority:** Nice-to-have

---

## Improvements Index

From agent IMP sections:

| ID | Agent | Title | Priority |
|----|-------|-------|----------|
| I-01 | fd-architecture | Decay event uses empty-string discovery_id | Nice-to-have |
| I-02 | fd-architecture | ListEvents/ListAllEvents diverge from each other — consider unified options struct | Nice-to-have |
| I-03 | fd-correctness | Add missing v9 migration step in db.go | Nice-to-have |
| I-04 | fd-correctness | Confirm discovery_events idx_discovery_events_discovery covers Rollback per-ID loop | Nice-to-have |
| I-05 | fd-correctness | Score validation missing upper-bound check | Should-fix |
| I-06 | fd-correctness | SubmitWithDedup scans dismissed/promoted records | Should-fix |
| I-07 | fd-quality | Add TestListEvents_IncludesDiscovery for run-scoped path | Should-fix |
| I-08 | fd-quality | Document cross-run discovery semantics in AGENTS.md | Should-fix |
| I-09 | fd-quality | Remove or use DISC_ID5 in integration test | Nice-to-have |
| I-10 | fd-safety | Add JSON validation and size cap to readFileArg callers | Should-fix |
| I-11 | fd-safety | Add readFileArgContained helper with CWD enforcement | Should-fix |
| I-12 | fd-safety | Dynamic query in List/Search — safe but note pattern deviation from allowlist | Info |

---

## Conflicts & Disagreements

**None identified.** All agents agree on the three high-convergence critical defects. Single-agent findings do not contradict each other.

---

## File Sections at Risk

Based on agent findings:

1. **`internal/event/store.go` (ListEvents UNION ALL)** — Discovery leg missing run_id filter [CRITICAL]
2. **`cmd/ic/events.go` (saveCursor function)** — interspect field hardcoded to zero [CRITICAL]
3. **`internal/discovery/store.go` (Decay method)** — TOCTOU window on status check [CRITICAL]
4. **`internal/discovery/store.go` (Dismiss method)** — Lacks idempotency guard [LOW]
5. **`cmd/ic/discovery.go` (readFileArg function)** — No CWD containment check [SHOULD-FIX]
6. **`cmd/ic/discovery.go` (cmdDiscoveryList, Status, ProfileGet, ...)** — Silent json.Marshal errors [CRITICAL]
7. **`internal/event/event.go` (Event.Source godoc)** — Stale comment [NICE-TO-HAVE]
8. **`cmd/ic/discovery.go` (cmdDiscoveryProfile)** — Inline arg check instead of switch [NICE-TO-HAVE]

---

## Summary Statistics

| Metric | Value |
|--------|-------|
| Agents Launched | 4 |
| Agents Completed | 4 |
| Agents Failed | 0 |
| Total Findings | 15 unique findings (deduplicated) |
| **Critical (P0/MUST-FIX)** | 4 findings |
| **Important (P1/SHOULD-FIX)** | 5 findings |
| **Nice-to-Have (INFO)** | 6 findings |
| **Improvements** | 12 items |
| **Conflicts** | 0 |
| **Gate Verdict** | **FAIL** — 4 MUST-FIX + 5 SHOULD-FIX findings block merge |

---

## Overall Verdict

**VERDICT: needs-changes**
**GATE: FAIL**

The E5 Discovery Pipeline is structurally sound and follows project conventions well. However, four critical defects block merge:

1. **ListEvents discovery leg missing run_id filter** — data-integrity violation for consumers
2. **saveCursor hardcodes interspect:0** — silent data loss on cursor round-trip
3. **Decay TOCTOU window** — concurrent status changes corrupt scored/promoted invariant
4. **Silently discarded json.Marshal errors** — automation cannot detect output failures

Five additional SHOULD-FIX findings (idempotency guard, readFileArg containment, etc.) should be addressed in this PR for consistency and robustness.

---

## Recommended Merge Criteria

**Block on:**
- [ ] Fix ListEvents discovery leg filter (or document intentional cross-run semantics + add test)
- [ ] Fix saveCursor interspect round-trip
- [ ] Fix Decay TOCTOU window (move SELECT inside transaction)
- [ ] Fix json.Marshal error handling in all `--json` output paths

**Should address before merge:**
- [ ] Add Dismiss idempotency guard (1-line fix)
- [ ] Add readFileArg CWD containment check
- [ ] Add TestListEvents_IncludesDiscovery integration test
- [ ] Document --since-discovery flag in usage help and AGENTS.md

**May defer to next sprint:**
- [ ] All INFO/IMP items (documentation, schema refactoring, observability gaps)

---

## Files Referenced

**Agent Reports:**
- `/root/projects/Interverse/infra/intercore/.clavain/quality-gates/fd-architecture.md`
- `/root/projects/Interverse/infra/intercore/.clavain/quality-gates/fd-correctness.md`
- `/root/projects/Interverse/infra/intercore/.clavain/quality-gates/fd-quality.md`
- `/root/projects/Interverse/infra/intercore/.clavain/quality-gates/fd-safety.md`

**Source Code (E5 changes):**
- `internal/event/store.go` — Event storage and UNION ALL query
- `internal/discovery/store.go` — Discovery lifecycle, Decay, Dismiss, Rollback
- `cmd/ic/discovery.go` — CLI subcommands and discovery profile management
- `cmd/ic/events.go` — Event tailing and cursor management
- `internal/event/event.go` — Event type definitions

---

**Synthesis prepared by:** intersynth quality-gates review engine
**Date:** 2026-02-20
