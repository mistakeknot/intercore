---
artifact_type: contract
beads: [mk-qozg, Sylveste-i385]
goal: d1ad6d9b
date: 2026-09-14
revision: 3.1
status: frozen
base: intercore 5c34dd3, Clavain 4d52755
history: revisions 1 and 2, the reviews, probes and review ledger are kept in the private goal record, not in this repository
---

# Completion-driven dispatch handoffs — kernel contract

Revision 3 is revision 2 with mk's rulings D1–D11 folded in; revision 3.1 applies mk's erratum on the terminal-record delete guard. It is frozen: changes need a new ruling recorded in the goal charter and a revision bump.

## 0. Provenance

- **Revision 1** was reviewed by a governed Fable 5.1 plan-review seat (dispatch `32CBB09D-4F09-4E18-AE8A-4AA818F20FC2`, route records 617/618, producer `claude-opus-5`, `validator_relationship: different-model`, policy `8148151129e3`; mk chose Fable because Astra/Sol usage was exhausted) and by three adversarial lens reviews, two probe-backed on macOS. flux-melange was attempted and failed on subscription auth.
- The reviews disproved revision 1's wake: a lifetime file lock was held through a read-only descriptor, released by Go's GC finalizer, and paired with a worker-forgeable spool. A producer probe showed macOS kqueue `EVFILT_PROC`/`NOTE_EXIT` waking on a non-child, sandboxed process's exit.
- **Revision 2** redesigned around process-exit notification and a single-writer supervisor. mk ruled D1–D11 on 2026-09-14.
- That record is kept privately with the goal charter rather than in this repository.

## 1. Problem

Coordinators learn that delegated work finished by polling: Clavain's review and prepare supervisors loop every second (`review_worker.go:510-573`, `prepare_worker.go:418-460`) and LLM guidance re-lists an output directory every 30 seconds (`commands/clavain-review.md:67`). 243 waiting-related calls consumed 43.66M processed tokens, 27.70M from agent polling. The target lifecycle is: dispatch → originator yields → terminal outcome recorded once → originator resumes from that record. mk ruled kernel-first.

## 2. Intercore today (5c34dd3)

| Area | Today |
|---|---|
| Terminal event | `Store.UpdateStatus` commits the CAS, then fires the recorder outside the transaction; a crash loses the event |
| Who terminalizes | `Spawn` returns after `cmd.Start()` (`spawn.go:158-178`); only Poll, Collect, Kill, CancelByRun and spawn-failure paths transition to terminal — the nine sites `spawn.go:138,144,159,172`, `collect.go:121,210,225`, `worker_receipt.go:211`, `dispatch.go:532-536` |
| Evidence | Non-flere Collect infers exit from the `.verdict` file (`collect.go:111-121, 315-328`); Kill on a dead pid calls Collect and can end `completed` (`collect.go:193-194, 200-201`) |
| `events tail --follow` | DB polling loop, 500 ms (`cmd/ic/events.go:126-182`); run-scoped cursors always get a 24 h TTL; no dispatch filter; no `dispatch_id` in output |
| `dispatch wait` | Ticker over Poll (`collect.go:125-173`); a fired timeout kills the worker (`collect.go:148`); a concurrent Collect exits 2 (`collect.go:69-71`) |
| `--timeout=` | Swallowed globally as the SQLite busy timeout (`cmd/ic/main.go:46-53`). Dead as a result: `dispatch spawn --timeout` (Clavain `tests/test_flere_seam.py:41`), `dispatch wait --timeout` (`hooks/lib-intercore.sh:168-178`, no live callers), `lock acquire --timeout` (`claim.go:71,220`, `runtime_evidence.go:204`) |
| Busy timeout | CLI default 100 ms (`main.go:75-76`); library default 5 s unused by the CLI |
| Admission | Sums live `input_tokens + output_tokens` (`admission.go:195`) |
| Transactions | modernc ignores `TxOptions.Isolation`; immediate transactions need explicit `BEGIN IMMEDIATE` on a held connection (`admission.go:98-106`); `foreign_keys` is set per connection (`db.go:68`) |
| Idempotency | No unique constraint on `dispatch_events` |
| Exit codes | Code: 3 usage, 2 internal, 1 expected rejection. `COMPATIBILITY.md` states 2 and 3 reversed |
| DB location (this host) | `~/.clavain/intercore.db`, writable from Clavain's review sandbox |

## 3. Invariants

- **I1 At most one terminal record.** At most one `dispatch_terminals` row per dispatch attempt. A row written by `terminalize` commits in the same transaction as the status change and one terminal event. A safety-net trigger row also carries a terminal event and is marked `unrecorded`.
- **I2 Record before wake, crash-durable (D9).** No waiter is told an outcome before its transaction commits. Records are durable across process and OS crashes. On macOS a sudden power loss can roll back the most recent records while their external effects (commits, pushes) remain; SQLite sync settings are unchanged.
- **I3 Evidence decides success.** `await` reports success only for `completed` with `evidence_status` `verified`, or `not_applicable` for dispatches spawned without `--require-report`. `completed` with `unrecorded` evidence is UNVERIFIABLE.
- **I4 Claimed-once consumption.** At most one live consumption per `(consumer, dispatch_id)` and per `(consumer, run_id, stage_key)`. `--consumes` admits a successor atomically with its consumption. External effects are once only if the consumer claims before acting.
- **I5 No silent recovery.** No automatic retry, model change, compensating unclaim or gate bypass on any terminal path. Stage takeover happens only through an explicit, audited `supersede` (D7).
- **I6 Deadlines never kill implicitly.** An `await` deadline returns control and touches nothing. (`dispatch wait` keeps its legacy kill-on-deadline, D6.)
- **I7 Single writer (D2).** While a supervisor lives it is the only writer of its dispatch's terminal record. Any other writer must first prove the supervisor dead by recorded identity.
- **I8 Polling is named.** Any remaining poll is a named, bounded fallback outside the canary path.

## 4. Data model (schema 040)

```sql
CREATE TABLE dispatch_terminals (
  dispatch_id TEXT PRIMARY KEY,
  run_id TEXT NOT NULL DEFAULT '',
  attempt INTEGER NOT NULL,
  parent_dispatch_id TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL CHECK (status IN ('completed','failed','timeout','cancelled')),
  from_status TEXT NOT NULL,
  terminal_source TEXT NOT NULL CHECK (terminal_source IN ('supervisor','reconcile','collect','spawn','cancel_by_run','kill','spool','trigger','legacy')),
  exit_code INTEGER, exit_signal TEXT,
  failure_class TEXT,
  evidence_status TEXT NOT NULL CHECK (evidence_status IN ('verified','missing','malformed','mismatched','not_applicable','unrecorded')),
  usage_status TEXT NOT NULL DEFAULT 'unknown' CHECK (usage_status IN ('complete','incomplete','unknown')),
  input_tokens INTEGER, output_tokens INTEGER, cache_read_tokens INTEGER, cache_write_tokens INTEGER,
  producer_backend TEXT, producer_model TEXT, route_decision_id INTEGER,
  report_path TEXT, report_sha256 TEXT,
  artifacts_json TEXT NOT NULL DEFAULT '{}',
  base_commit TEXT, base_worktree_digest TEXT, head_commit TEXT, head_worktree_digest TEXT,
  orphans_killed INTEGER NOT NULL DEFAULT 0,
  contested INTEGER NOT NULL DEFAULT 0,
  event_id INTEGER NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE TABLE dispatch_supervision (
  dispatch_id TEXT PRIMARY KEY,
  db_path TEXT NOT NULL,
  supervisor_pid INTEGER, supervisor_birth TEXT,
  worker_pid INTEGER, worker_birth TEXT,
  prompt_sha256 TEXT NOT NULL,
  base_commit TEXT, base_worktree_digest TEXT,
  report_path TEXT, observe_lock_path TEXT,
  deadline_unix INTEGER,
  created_at INTEGER NOT NULL
);
CREATE TABLE dispatch_intents (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  dispatch_id TEXT NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN ('cancel','run_rollback','deadline')),
  reason TEXT, requested_by TEXT, created_at INTEGER NOT NULL
);
CREATE TABLE dispatch_consumptions (
  consumer TEXT NOT NULL, dispatch_id TEXT NOT NULL,
  run_id TEXT NOT NULL DEFAULT '', stage_key TEXT NOT NULL DEFAULT '',
  effect_kind TEXT, effect_ref TEXT,
  superseded_by TEXT, supersede_reason TEXT,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (consumer, dispatch_id)
);
CREATE UNIQUE INDEX ux_consumptions_stage ON dispatch_consumptions(consumer, run_id, stage_key)
  WHERE stage_key <> '' AND superseded_by IS NULL;
CREATE TABLE dispatch_terminal_deliveries (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  consumer TEXT NOT NULL, dispatch_id TEXT NOT NULL,
  outcome TEXT NOT NULL, created_at INTEGER NOT NULL
);
```

Triggers:
- **Safety net** — `AFTER UPDATE OF status ON dispatches`, non-terminal → terminal, no terminal row: insert a terminal `dispatch_events` row and a `dispatch_terminals` row with `INSERT … SELECT … WHERE NOT EXISTS`, every NOT NULL column filled in SQL (`attempt = NEW.retry_count`, `from_status = OLD.status`, `created_at = unixepoch()` — a documented exception to the Go-timestamp rule), `terminal_source='trigger'`, `evidence_status='unrecorded'`.
- **Supervision guard** — `BEFORE UPDATE OF status ON dispatches`, non-terminal → terminal, row has `dispatch_supervision`, no terminal row: `RAISE(ABORT)`. `terminalize` inserts its row first and passes; legacy Collect, old binaries and raw SQL fail loudly.
- **Immutability** — `BEFORE UPDATE ON dispatch_terminals`: abort. `BEFORE DELETE ON dispatch_terminals`: abort while a `dispatches` row with that id still exists, so a terminal record can only be removed after its dispatch (erratum 3.1; a persistent trigger cannot reference the temp schema).
- **No REPLACE** — every status write on `dispatches` is a plain `UPDATE`; a test fails on any `OR REPLACE`/`REPLACE INTO dispatches` (an outer REPLACE overrides trigger conflict clauses and bypasses update-only immutability; probe T4).

Two-release ship (D10): release N raises `maxSchemaVersion` to 40 and references no new table; release N+1 migrates and enables `--supervise`, which refuses below schema 40. Prune removes new rows, supervision state and spool files for pruned dispatches in one transaction, deleting each dispatch row before its terminal record.

## 5. Mechanisms

### 5.1 Terminal writer

`terminalizeTx(q querier, id, to, fields, evidence)` touches only `q` — never `s.db`, `event.Store`, `Store.Get` or `loadReasoningDecision` — so it cannot deadlock the single connection. `terminalize` holds a connection, runs `BEGIN IMMEDIATE`, calls `terminalizeTx`, commits or rolls back.

In the transaction, in order:
1. Read `status` and `retry_count`; if terminal, return the existing row (`already_terminal`).
2. Insert the terminal `dispatch_events` row; keep `last_insert_rowid()`.
3. Insert the `dispatch_terminals` row with that `event_id` and the actual previous status.
4. `UPDATE dispatches SET status=? … WHERE id=? AND status NOT IN ('completed','failed','timeout','cancelled')`; if `changes()=0`, roll back and re-read.

A primary-key conflict at step 3 means another writer won: roll back and return the existing row. Evidence is computed before `BEGIN`. `CancelByRun` wraps all its rows in one immediate transaction calling `terminalizeTx` per row. All nine existing terminal sites and the new writers use it. Probe-verified: this order commits, the safety net is ignored, a zero-row CAS rolls back cleanly, immutability aborts updates.

### 5.2 Supervised spawn (opt-in, CLI only)

`ic dispatch spawn --supervise [--require-report] [--deadline=<dur>] [--observe-lock=<path>]` (release N+1). Refused for in-process library callers (`dispatch_executor.go`, `run_lifecycle.go`) until audited.

1. **Admission** records the dispatch and its `dispatch_supervision` row: absolute symlink-free DB path, full prompt sha256, report path, observe-lock path, deadline.
2. The spawner starts hidden `ic dispatch supervise <id> --db=<abs>` with `Setsid` and a status pipe, with close-on-exec on every inherited descriptor ≥3 except the pipe, and closes its copy of the pipe's write end right after start. Handshake bounded (default 10 s); on timeout, prove the supervisor dead and write `failed/spawn_lost`.
3. The supervisor sets close-on-exec on all descriptors ≥3, records its pid and birth identity, observes the base tree, starts the worker with `Setpgid`, records worker identity and moves `spawned → running` in one immediate transaction, then writes `ok <pid>`. If that CAS fails (cancelled meanwhile) it kills the worker group and exits.
4. It waits for worker exit and for intents (§5.6). On worker exit: detect the leader's death with `waitid(WEXITED|WNOWAIT)`, enumerate the process group and session, signal remaining members under the witness rule (`collect_unix.go:21-36`), confirm the group empty by enumeration, reap the leader, verify the report (§5.8), take the observe lock, observe the head tree, `terminalize`, release the lock, exit.
5. The supervisor holds only plain integer descriptors it opened itself; nothing it depends on is wrapped in an `*os.File` that GC could finalize.
6. **Deadline (D5):** one timer; on expiry it records a `deadline` intent and handles it as §5.6, classifying `deadline_expired`.
7. **Tree binding (D3):** the worker is bound at the leader only. Full tree binding is deferred to `Sylveste-i385.16`.

### 5.3 Wake: process-exit notification (D1)

Awaiters wait on the supervisor's exit, never on a file:
- **macOS:** kqueue `EVFILT_PROC | NOTE_EXIT` on `supervisor_pid` (probe-verified: non-child pid, sandboxed target, deadline return, `ESRCH` on an exited pid).
- **Linux:** `pidfd_open(supervisor_pid)` + `poll` (to verify on zklw in K4).

PID-reuse guard: register the watch first, then compare birth identity with `supervisor_birth`; mismatch or `ESRCH` means the supervisor is gone.

### 5.4 `ic dispatch await <id> [--consumer=<name>] [--run=<run_id>] [--deadline=<dur>]`

1. Terminal row exists → deliver.
2. No `dispatch_supervision` row and not terminal → legacy dispatch, exit 7.
3. Wait on the supervisor against one deadline timer. Deadline → record a `deadline` delivery; if the supervisor is alive by identity, exit 4; if dead, reconcile. Wake → reconcile.
4. **Reconcile** (supervisor proven dead): import a spool if present (§5.5); if still no terminal row, enumerate the recorded worker group; members alive → exit 7 (`supervisor_lost_worker_alive`; recovery is explicit `ic dispatch kill`); group empty → sweep nothing further and `terminalize(failed, supervisor_lost, evidence missing, terminal_source=reconcile)`. The sweep of survivors happens only in `ic dispatch kill` (D3).
5. **Deliver** JSON: the terminal row, post-terminal events with `contested`, `consumption` state, `delivery: first|duplicate`; record the delivery.

`--run` must equal `scope_id`, else exit 6.

Exit codes (new command only): **0** success; **8** terminal, not success (`failed`/`timeout`/`cancelled`, class in JSON); **9** terminal, unverifiable (`completed` + `unrecorded`); **4** deadline, supervisor alive; **5** not found; **6** run mismatch; **7** supervisor lost with worker alive, or legacy non-terminal; **3** usage; **2** internal.

A coordinator issues exactly one await per dispatch per incarnation. Clavain calls it through a runner with no timeout (not `reviewRun`'s 45 s context). LLM coordinators run it as a background task reported by the host on exit; a foreground tool call capped below the dispatch's lifetime is not a supported canary path.

### 5.5 Busy database and spool

The supervisor opens the DB with a 30 s busy timeout and one named capped retry helper `terminalRetry` (reported as `db_busy_retries`). Errors are classified: busy → retry; schema-too-new, constraint violation, disk full → minimal `failed/terminal_record_rejected` row if possible, else spool. The spool is `<dbdir>/dispatch/<id>.terminal.json` (directory 0700, each component opened with `O_NOFOLLOW`), written temp → fsync → rename → fsync directory. **A spool never produces `completed`:** the importer re-verifies report and artifacts against what `dispatch_supervision` recorded before the worker started; otherwise `failed/spool_unverified`. The spool is deleted after its row commits. Library Poll/Collect import spools too.

### 5.6 Intents: kill, rollback, deadline (D2)

- `ic dispatch kill <id>` on a supervised dispatch records a `cancel` intent and signals the supervisor (`SIGUSR1`) under its recorded identity, then waits on the supervisor's exit (§5.3) for up to 60 s. If the supervisor is proven dead without a terminal row, kill enumerates the recorded group, signals survivors under the witness rule, confirms empty, and writes `failed/supervisor_lost` through `terminalize`. Kill never writes on a timer while the supervisor lives.
- `CancelByRun` records `run_rollback` intents for supervised rows and signals their supervisors; unsupervised rows keep today's single-transaction cancel via `terminalizeTx`.
- **Precedence (D2, order decides):** if the worker exited with a verified outcome before the intent committed, the record is `completed` and the intent is kept as a `contested` post-terminal event; if the intent committed first, the record is `cancelled` (or `timeout` for `deadline`) and any later verified outcome is kept as a contested event. A signal death with no intent is `killed_externally`.
- `ic dispatch kill` on an already-terminal dispatch reports the actual terminal status and records the attempt as a delivery row.

### 5.7 Consumption (D7)

- `ic dispatch ack <id> --consumer --effect=<ref> [--run] [--stage]` is a **claim before the effect**: plain `INSERT`; outcomes `first` (proceed), `already` (abort; `effect_ref` names the winner), `stage_taken`, `run_mismatch`.
- `ic dispatch spawn … --consumes=<prev> --consumer --stage` inserts the consumption inside the admission transaction; `<prev>` must be terminal; replay checks both `(consumer, dispatch_id)` and `(consumer, run_id, stage_key)` and returns the existing successor with `replayed: true`.
- `ic dispatch supersede <old> --by=<new> --consumer --reason`: allowed only when `<old>` is terminal and not success and `<new>` is in the same run; one immediate transaction sets `superseded_by` and `supersede_reason` on the old consumption, inserts the new one and records an audit event. At most one non-superseded consumption per stage.
- Resume after restart: re-await the last dispatch; if consumed, follow `effect_ref`; otherwise claim with `ack` before acting.

### 5.8 Worker report and trust boundary (D4)

`ic.dispatch-report.v1` fields: `schema`, `dispatch_id`, `attempt_id`, `attempt`, `run_id`, `submitted_prompt_sha256`, `outcome`, `failure_class`, `backend_exit`, `provider`, `model`, `usage{input,output,cache_read,cache_write}_tokens`, `usage_status`, `artifacts{name:{path,sha256}}`, `tree{head_commit,worktree_digest}`, `route_decision_id`. Written to `dispatch_supervision.report_path`, which must be writable by the worker's sandbox and outside the kernel state directory; the report must not exist before spawn.

Kernel checks: exact schema; `dispatch_id == attempt_id == id`; `attempt == retry_count`; `run_id == scope_id`; `submitted_prompt_sha256` equals the recorded sha256; model agrees with the routing decision when present; artifact paths absolute with matching sha256. The kernel finds the terminal route record itself and observes the tree itself (§5.2 step 4); claimed ≠ observed ⇒ `evidence_mismatched`. Precedence: `supervisor_lost` > `evidence_*` > report class > `backend_exit_nonzero`.

**`verified` means bound to this dispatch and consistent with kernel observation. It does not mean attested:** a worker that can write outside the project can write its own report. Clavain sandbox profiles deny read and write on the DB file and `<dbdir>/dispatch/`. Adapter attestation is deferred to `Sylveste-i385.17`.

### 5.9 Budget and usage (D5)

The usage wrapper — `scripts/claude_usage.py` for Claude seats and a new `scripts/codex_usage.py` parent wrapper for Codex seats (`codex_usage.py --budget N -- codex exec --json`) — enforces the token budget at turn boundaries, kills its own model child, records per-turn overshoot, writes a `usage.cumulative` v1 ledger with a fsynced terminal record before exit, and pushes cumulative usage to the kernel after each completed turn (`ic dispatch tokens`), so admission (`admission.go:195`) keeps seeing live spend. The supervisor holds the external wall-clock deadline (§5.2 step 6).

## 6. Cases

| Case | Terminal row | await exit | Consumer |
|---|---|---|---|
| Success, report verified | `completed`, verified | 0 | advance via `--consumes` |
| Worker failure / classified | `failed`, class by precedence | 8 | stop; no retry |
| Report missing / malformed / mismatched | `failed`, `evidence_*` | 8 | stop |
| Tree claim ≠ observation | `failed`, `evidence_mismatched` | 8 | stop; candidate not advanced |
| Usage required, incomplete | `failed`, `usage_incomplete`, lower-bound tokens | 8 | stop; never zero |
| Budget reached in wrapper | `failed`, `budget_exhausted`, overshoot recorded | 8 | stop |
| Await deadline, supervisor alive | none | 4 | yield again or escalate |
| Supervisor deadline | per D2 precedence, `deadline_expired` | 8 | stop |
| Kill, supervisor alive | per D2 precedence | 8 | stop |
| Kill, supervisor dead | survivors swept, `failed/supervisor_lost` | 8 | stop |
| Run rollback | per D2 precedence; unsupervised rows `cancelled` in one tx | 8 | stop; current awaiters wake |
| Supervisor killed, group empty | `failed/supervisor_lost` via reconcile | 8 | stop; never success |
| Supervisor killed, worker alive | none | 7 | explicit kill, then await |
| Spawner or supervisor dies before handshake | `failed/spawn_lost` | 8 | stop |
| Legacy Collect / old binary on a supervised row | none (guard aborts) | — | loud error |
| Raw status UPDATE outside `terminalize` | trigger row, `unrecorded` | 9 if `completed`, else 8 | stop |
| DB busy at terminal | spool imported; never `completed` without re-verification | 0 or 8 | normal |
| Forged spool | `failed/spool_unverified` | 8 | stop |
| Duplicate await | unchanged | same code, `delivery=duplicate` | no re-run |
| Replay after restart | unchanged | same | follow `effect_ref` |
| Failed stage retried | old consumption superseded by explicit `supersede` | — | audited takeover |
| Wrong run | unchanged | 6 | reject |

## 7. Compatibility

- **Additive:** new tables and triggers; `await`, `ack`, `supersede`, hidden `supervise`; `spawn --supervise|--require-report|--consumes|--consumer|--stage|--deadline|--observe-lock`; terminal event type; `dispatch_id` in event JSON; `events tail --dispatch|--until-terminal`; new exit codes on new commands only.
- **`dispatch wait` (D6):** semantics unchanged, including kill on deadline and exit codes. K0 fixes only the concurrent-collect exit 2. Documented as the legacy killing, polling path; `await` is the non-killing, non-polling path.
- **Flags:** `--busy-timeout=` added; `--timeout=` routed to `dispatch spawn`, `dispatch wait` and `lock acquire`; CLI default busy timeout raised from 100 ms to 5 s. Release notes: routed `--timeout=` now takes effect (on `wait` it kills at the deadline); `lock acquire` honors the requested 500 ms / 2 s instead of the silent 1 s default.
- **Supervision guard** makes legacy writers fail loudly on supervised rows only.
- **Patch fixes:** Poll/Wait re-read and return the terminal row on ErrStaleStatus; `foreign_keys(1)` in the DSN; `coordination/store.go` begins immediate transactions explicitly; `COMPATIBILITY.md` exit-code table corrected to the code.

## 8. Rollout (D10)

After release N+1 merges: install on the Mac, run `ic init`, run the canary against the installed and worktree binaries, soak through at least one real multi-stage Clavain run; then install and `ic init` on zklw, with mk running the `ssh zklw` steps. Until installed, tests and the canary pin a worktree-built `ic` by absolute path. The `--supervise` default flip is decided at the post-soak checkpoint.

## 9. Polling definition and audit (D11)

A **coordinator polling call** is any observation of delegated work's state made by the originator, or by any process or tool acting for it, after dispatch and before the terminal record is delivered, other than the single blocking `ic dispatch await` per dispatch per coordinator incarnation. It includes:
1. any `ic` invocation reading dispatch, run, event or budget state for that work;
2. any process other than `ic` opening the intercore DB file;
3. any Clavain command reporting on the work (`clavain-cli review status`, `prepare status`) and any read of its receipt, output or usage files;
4. any LLM tool call targeting the work's output, receipt or DB directories, and any tool whose function is to report another agent's or process's status (background-task output readers, monitors, agent-introspection MCP tools);
5. any exec of a timing or inspection utility in that window;
6. every await beyond the first and every deadline delivery.

**Audit on the Mac (D11):** the canary's coordinator process tree runs under a `sandbox-exec` profile that denies `process-exec` except `ic`, the dispatch scripts and the model CLIs each stage needs, so a polling exec fails with a sandbox violation. Also: coordinator CPU time between spawn return and delivery (catches builtin spin loops), transcript tool calls, `dispatch_terminal_deliveries`, and a Go test forbidding `time.Sleep`/tickers in await and supervise code outside `terminalRetry` and the kill escalation in `collect_unix.go:37-45`. The Linux leg may add `strace -f` if unprivileged tracing works on zklw. `db_busy_retries` and worker-internal timers are reported separately.

**Named fallbacks** (outside the canary path): Clavain's claim-staleness sweep (`watchdog.go:134-146`); interflux `flux-watch.sh`'s 5 s loop without `inotifywait`; legacy unsupervised dispatches.

## 10. Clavain replacement map

| Site | Replacement |
|---|---|
| `review_worker.go:510-573` (model worker) | spawn `--supervise --require-report --deadline=1h --observe-lock=<project lock>`; one `ic dispatch await` through a no-timeout runner; stop only via `ic dispatch kill` |
| `review_worker.go:574-631` (verification) | **D6-bis:** a second supervised dispatch that `--consumes` the worker dispatch runs the approved checks and build and hashes the retest binary; own terminal record, deadline and receipt; the coordinator awaits it last |
| `prepare_worker.go:418-460` | same as the worker row with `--deadline=15m`; policy hash verified before spawn and bound in the report |
| `review_usage.go:210-261`, `kernel_report_pending` | usage and `usage_status` from the terminal record; `kernel_report_pending` retired for supervised dispatches; the wrapper's fsynced terminal usage record is written before exit |
| launch.sh dispatch-id file wait (`review_worker.go:446`, `prepare_worker.go:370`) | removed; spawn exports `IC_DISPATCH_ID` (`spawn.go:156-157`) |
| `review.go:411-431`, `prepare.go:444-460` status re-polls | callers await; status calls count as polling |
| `daemon.go:156-218, 458-492` | done channel from the `cmd.Wait()` goroutine |
| codex path in `scripts/dispatch.sh` | `scripts/codex_usage.py` parent wrapper (§5.9) |
| `commands/clavain-review.md:67` and sibling guidance | background `ic dispatch await`; no directory polling |
| sandbox profiles (`review_worker.go:431-438`, `prepare_worker.go:356-361`) | deny read/write on the DB file and `<dbdir>/dispatch/` |

## 11. Slices and named tests

- **K0** (`Sylveste-i385.9`): ErrStaleStatus re-read; `COMPATIBILITY.md` exit codes; wait/await documentation. Test: two collectors of one dead dispatch both get the terminal row.
- **K1** (`.10`): `--busy-timeout`, `--timeout=` routing, 5 s default. `TestBusyTimeoutDefaultAndRouting`.
- **K2** (`.11`): release N max=40; schema 040, `terminalizeTx` at all terminal sites, triggers. `TestTerminalizeOrderSurvivesImmutabilityTrigger`, `TestSupervisedDispatchNeverVerdictInferred`, `TestNoReplaceOnDispatches`, `TestTerminalRowDeleteNeedsDispatchGone`, `TestTriggerRowCarriesEventAndColumns`, `TestTerminalizeTxNoNestedStoreCalls`, `TestCancelByRunAtomic`, `TestForeignKeysSurviveReconnect`, `TestCoordinationBeginsImmediate`.
- **K3** (`.12`): `await` (legacy mode), `ack`, `--consumes`, `supersede`. `TestAwaitRefusesUnrecordedCompleted`, `TestConsumptionNullRunStageUnique`, `TestAckClaimBeforeEffect`, `TestConsumesReplayBothKeys`, supersede refusal and late-attempt tests.
- **K4** (`.13`, riskiest): supervisor, process-exit wake, intents, reconcile, spool. `TestSpawnLostBeforeHandshake`, `TestSupervisorSweepsDoubleForkedGrandchild`, `TestSupervisorKilledWorkerAliveExit7`, `TestKillWaitsForSupervisorWrite`, `TestForgedSpoolNeverCompletes`, `TestNoInheritedDescriptorsInWorker`, `TestSupervisorSurvivesForcedGC`, `TestAwaitPidReuseGuard`, `TestAwaitDeadlineTouchesNothing`, `TestIntentPrecedenceOrderDecides`, `TestObserveLockHeldAcrossObservation`; macOS and zklw Linux.
- **K5** (`.14`): report verifier, kernel route lookup and tree observation, `--require-report`. One fixture per binding failure; `TestTerminalUsageFlushedBeforeExit`.
- **K6** (`.15`): events tail `dispatch_id`, `--dispatch`, `--until-terminal`, cursor TTL fix.
- **Deferred:** tree binding `.16` (D3), attestation `.17` (D4).

## 12. Rulings (mk, 2026-09-14)

| # | Ruling |
|---|---|
| D1 | Adopt revision 2: process-exit wake, single-writer supervisor, SQL supervision guard, evidence-gated success, structural polling audit |
| D2 | Order decides between a verified completion and a cancel intent; the loser is a contested post-terminal event |
| D3 | Leader-bound now with reconcile/kill sweep; full tree binding deferred (`Sylveste-i385.16`) |
| D4 | Report bound, honestly worded; `verified` ≠ attested; sandboxes deny kernel state; attestation deferred (`Sylveste-i385.17`) |
| D5 | Wrapper enforces token budget per turn and pushes usage per turn; supervisor holds the wall-clock deadline; new `codex_usage.py` |
| D6 | `dispatch wait` unchanged except the stale-collect fix; `await` is the non-killing path |
| D6-bis | Clavain verification is its own supervised dispatch consuming the worker dispatch |
| D7 | Explicit `ic dispatch supersede` for failed stages |
| D8 | Tree observed under the caller-supplied project lock |
| D9 | Crash-durable only, stated honestly |
| D10 | Mac first, zklw after soak; two-release ship with explicit `ic init` |
| D11 | Canary coordinator under an exec-deny `sandbox-exec` profile, plus CPU accounting, transcript, deliveries and the Go no-sleep test |
| Cards | Intercore and Clavain product cards confirmed |
| CI | Ask the owning session of `mk-u59j` before any takeover; coding proceeds |
| Carried | Astra/Sol exhausted → Fable reviewer; Codex-exhaustion fallback tracked in `mk-9yyt`; `--supervise` default flip at post-soak checkpoint |
| 3.1 | Terminal records are deletable only after their dispatch row is gone (dispatch-row delete guard), replacing `temp.prune_authorized`, which SQLite cannot reference from a persistent trigger |
