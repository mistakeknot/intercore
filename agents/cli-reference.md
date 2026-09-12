# CLI Reference

Complete `ic` command reference, flags, and exit codes.

## Architecture

```
cmd/ic/          CLI entry point + domain handlers (run, dispatch, gate, lock, events, coordination, scheduler, lane, discovery, portfolio, cost, usage, interspect, agency, action, config, publish, route, landed, session, situation)
internal/        Domain packages — key ones include db/, state/, dispatch/, phase/, event/, usage/, coordination/, scheduler/, lane/, discovery/, portfolio/, budget/, lock/, agency/, audit/, redaction/, publish/
lib-intercore.sh Bash wrappers for hooks (45 functions)
```

## CLI Commands

### Core

```
ic init                                    Create/migrate the database
ic health                                  Check DB readable, schema current, disk space
ic version                                 Print CLI and schema versions
ic compat status                           Show legacy temp file vs DB coverage
ic compat check <key>                      Check if key has data in DB
```

### State & Sentinels

```
ic state set <key> <scope> [--ttl=<dur>]   Write JSON (stdin or @filepath)
ic state get <key> <scope>                 Read JSON (exit 0=found, 1=not found)
ic state delete <key> <scope>              Remove a state entry
ic state list <key>                        List scope_ids for a key
ic state prune                             Remove expired state entries
ic sentinel check <name> <scope> --interval=<sec>   Atomic claim (exit 0=allowed, 1=throttled)
ic sentinel reset <name> <scope>           Clear a sentinel
ic sentinel list                           List all sentinels
ic sentinel prune --older-than=<dur>       Remove old sentinels
```

### Dispatch

```
ic dispatch spawn [flags]                  Spawn an agent dispatch (prints ID)
ic dispatch status <id>                    Show dispatch details
ic dispatch list [--active] [--scope=<s>]  List dispatches
ic dispatch poll <id>                      Check liveness, update stats
ic dispatch wait <id> [--timeout=<dur>]    Block until terminal or timeout
ic dispatch kill <id>                      SIGTERM then SIGKILL a dispatch
ic dispatch reconcile <id>                 Append later Flere terminal proof; preserve the original attempt
ic dispatch tokens <id> --set --in=N --out=N [--cache=N]   Update token counts
ic dispatch prune --older-than=<dur>       Remove old terminal dispatches
```

`dispatch spawn --run-id=<id>` requires an existing run and binds the dispatch
scope to it. A different `--scope-id` is invalid. Parent scope/depth, stored enforced
budget, stored agent cap, caller concurrency limits, and insertion are checked in
one SQLite write transaction. An advisory budget remains advisory. A configured
legacy budget checker can veto enforced admission but cannot replace the stored
budget. Active work can still consume tokens after admission; admission is not a
reservation of an estimated token allotment.

Stored `kernel.global_max_dispatches` and `kernel.max_spawn_depth` limits apply
in that same transaction and cannot be relaxed by caller flags. Invalid stored
limits fail admission. A scoped retry must still reference an existing run and
rechecks stored limits; it preserves the original depth while reserving a new
attempt. This includes `dispatch retry --escalate`; Flere and indeterminate
attempts are ineligible for either retry path. Retry, token, and kill rejections
return exit 1 and a structured reason under `--json`; unexpected storage errors
remain exit 2. Terminal Flere usage cannot be overwritten through `dispatch tokens`.
Host timeout or cancellation records a failed indeterminate Flere attempt;
later proof can be appended through reconciliation.

Spawn requires host process inspection before starting a child. On macOS this
includes `kern.proc.pid`, `kern.proc.all`, and `kern.bootsessionuuid`. A sandbox
denying these queries receives `process_identity_unavailable` (exit 1, structured
under `--json`); there is no weaker fallback. Hooks, CLI callers, and application
hosts must supply that permission in their actual execution context. No hook in
this package grants permissions or moves execution out of a caller's sandbox.

For a supported host invocation, prepare the explicit Flere executable/profile
as described in Clavain's `docs/guides/flere-worker.md`, then run from a normal
host terminal or application supervisor with process inspection permission:

```bash
cd "$PROJECT_ROOT"
ic --json dispatch spawn --run-id="$RUN_ID" --type=flere \
  --project="$PWD" --prompt-file="$PROMPT_FILE" --model="$FLERE_MODEL" \
  --sandbox=read-only --dispatch-sh="$CLAVAIN_SOURCE/scripts/dispatch.sh"
```

Running that command inside a restricted agent tool does not move it to the
host. A `process_identity_unavailable` rejection requires a permitted host
supervisor; changing policy, disabling checks, or retrying the same context
does not provide that permission.

Spawn records a versioned host birth identity in existing state under
`dispatch.process`, scoped by dispatch ID. Darwin uses the boot-session UUID and
the recorded fork time, so calendar changes do not change the birth. On macOS and
Linux, detached kill requires matching birth, PID, and group before signalling.
Group members observed before signalling can prove continued group ownership
after the leader exits during that termination call. A leader already gone before
inspection is collected; this does not prove cleanup of its orphaned descendants.

A successfully read different birth proves the original process exited: poll,
wait, and kill collect its evidence without signalling the new PID owner. Missing,
invalid, or legacy unversioned identities, denied inspection, and lost membership
do not prove exit. Cancellation then records an indeterminate failure and reports
`process_identity_unverified`; wait returns the terminal dispatch and exit 1.
Such attempts cannot automatically retry. Pruning an attempt also removes its
identity, while retained attempts keep theirs. These are host/application checks,
not an atomic operating-system handle for a Unix group; identities are rechecked
before each signal. Windows uses a creation-time-validated process handle and
TerminateProcess; Go does not implement sending os.Interrupt on Windows.

`--type=flere` forwards to Clavain's explicit fixed worker. It requires a run,
reviewed executable/profile, and a provider/model. It uses application-level
canonical-root read-only enforcement. Completion requires the host receipt bound
to this dispatch attempt and the native session's user/final assistant entries.
Missing or inconsistent evidence yields `failed` with
`failure_class=worker_outcome_indeterminate`; output text does not establish
success. No Flere attempt is automatically retried. Reconciliation appends an
event without rewriting the terminal attempt or granting task acceptance.

`dispatch spawn --scheduled` persists the complete spawn configuration. Queue
insertion grants no admission: execution must enter `dispatch.Spawn` again,
which reads current policy in the admission transaction. The scheduler library
requires an explicit executor; `scheduler.DispatchExecutor` decodes stored
options, invokes admission, collects terminal evidence, and disables replay.
Hosts own queue loading and persistence hooks. There is no scheduler daemon
consumer in this repository, so this is not live automatic scheduling proof.
Legacy queued snake_case maps are rejected rather than silently losing fields;
new `scheduler submit` records use the complete typed representation.

### Run

```
ic run create --project=<dir> --goal=<text> [--complexity=N] [--scope-id=S] [--phases='[...]'] [--token-budget=N] [--budget-warn-pct=N] [--budget-enforce] [--max-agents=N] [--actions='{}']
ic run status <id>                         Show run details
ic run advance <id> [--priority=N] [--disable-gates] [--skip-reason=S]
ic run phase <id>                          Print current phase (scripting)
ic run list [--active] [--scope=S] [--portfolio]  List runs
ic run events <id>                         Phase event audit trail
ic run cancel <id>                         Cancel a run
ic run current [--project=<dir>]           Print active run ID for project
ic run set <id> [--complexity=N] [--auto-advance=bool] [--force-full=bool] [--max-dispatches=N]
ic run skip <id> <phase> [--reason=<text>] [--actor=<name>]   Pre-skip a phase
ic run rollback <id> --to-phase=<phase> --reason=<text> [--dry-run]   Workflow rollback
ic run rollback <id> --layer=code [--phase=<p>] [--format=json|text]  Code rollback metadata
ic run tokens <id> [--project=<dir>] [--json]   Token aggregation across dispatches
ic run budget <id> [--json]                Check budget thresholds (exit 1=exceeded)
ic run agent add <run> --type=<t> [--name=<n>] [--dispatch-id=<id>]
ic run agent list <run>                    List agents for a run
ic run agent update <id> --status=<s>      Update agent status (active|completed|failed)
ic run artifact add <run> --phase=<p> --path=<f> [--type=<t>]
ic run artifact list <run> [--phase=<p>]   List artifacts for a run
ic run action add <run> --phase=<p> --command=<cmd> [--args=<json>] [--mode=<m>] [--type=<t>] [--priority=N]
ic run action list <run> [--phase=<p>]     List actions for a run
ic run action update <run> --phase=<p> --command=<cmd> [--args=<json>]
ic run action delete <run> --phase=<p> --command=<cmd>
```

### Gate

```
ic gate check <run_id> [--priority=N]      Dry-run gate evaluation (exit 0=pass, 1=fail)
ic gate override <run_id> --reason=<text>  Force-advance past a failed gate
ic gate rules [--phase=<p>]                Display gate rules table
```

### Lock (filesystem-only, no SQLite)

```
ic lock acquire <name> <scope> [--timeout=<dur>] [--owner=<id>]
ic lock release <name> <scope> [--owner=<id>]
ic lock list                               List all held locks
ic lock stale [--older-than=<dur>]         List stale locks
ic lock clean [--older-than=<dur>]         Remove stale locks (PID-liveness check)
```

### Events

```
ic events tail <run_id> [--consumer=<name>] [--follow] [--since-phase=N] [--since-dispatch=N] [--limit=N] [--poll-interval=<dur>]
ic events tail --all [flags]               Tail events across all runs
ic events record --source=<s> --type=<t> --payload=<json> [--run=<id>] [--session=<id>] [--project=<dir>]
  Sources: interspect, review, coordination, intent
  Payload validated per source (see below)
ic events cursor list                      List consumer cursors
ic events cursor reset <consumer>          Reset a consumer cursor
```

**`events record` payload requirements:**

| Source | Required fields | Notes |
|--------|----------------|-------|
| `interspect` | `agent_name` | Optional: `override_reason`, `context` |
| `review` | `finding_id`, `agents` (map), `resolution`, `chosen_severity`, `impact` | `--type` must be `disagreement_resolved` or `execution_defect` |
| `coordination` | `lock_id`, `owner`, `pattern`, `scope` | Optional: `reason` |
| `intent` | `intent_type`, `bead_id`, `idempotency_key` | Optional: `success` (bool), `error_detail` |

### Coordination (SQLite-backed, v20)

```
ic coordination reserve --owner=<o> --scope=<s> --pattern=<p> [--type=file_reservation|named_lock|write_set] [--ttl=<sec>] [--exclusive] [--reason=<text>] [--dispatch=<id>] [--run=<id>]
ic coordination release <id>               Release by lock ID
ic coordination release --owner=<o> --scope=<s>   Release by owner+scope
ic coordination check --scope=<s> --pattern=<p> [--exclude-owner=<o>]   Check for conflicts (exit 0=clear, 1=conflict)
ic coordination list [--scope=<s>] [--owner=<o>] [--type=<t>] [--active]
ic coordination sweep                      Expire TTL-based locks
ic coordination transfer <id> --to=<new-owner>   Transfer lock ownership
```

### Scheduler (v19)

```
ic scheduler submit --prompt-file=<f> --project=<dir> [--type=codex] [--session=<name>] [--name=<label>] [--priority=N]
ic scheduler status <job-id>               Check job status
ic scheduler stats                         Queue stats by status
ic scheduler list [--status=pending]       List jobs
ic scheduler cancel <job-id>               Cancel a job
ic scheduler pause                         Pause processing
ic scheduler resume                        Resume processing
ic scheduler prune --older-than=<dur>      Clean completed jobs
```

### Lane (v13)

```
ic lane create --name=<n> [--type=standing|arc] [--description=<d>]
ic lane list [--active] [--status=<s>]     List lanes
ic lane status <id-or-name>                Show lane details + members
ic lane close <id-or-name>                 Close a lane
ic lane events <id-or-name>                Lane event history
ic lane sync <id-or-name>                  Sync lane membership
ic lane members <id-or-name>               List bead members
ic lane velocity [--window=<days>]         Compute starvation/throughput scores
```

### Discovery (v9)

```
ic discovery submit --source=<s> --source-id=<id> --title=<t> [--score=<0-1>] [--summary=<s>] [--url=<u>] [--metadata=@<file>] [--embedding=@<file>]
ic discovery status <id> [--json]
ic discovery list [--source=<s>] [--status=<s>] [--tier=<t>] [--limit=N]
ic discovery score <id> --score=<0.0-1.0>
ic discovery promote <id> --bead-id=<bid> [--force]
ic discovery dismiss <id>
ic discovery feedback <id> --signal=<type> [--data=@<file>] [--actor=<name>]
ic discovery profile [--json]
ic discovery profile update --keyword-weights=<json> --source-weights=<json>
ic discovery decay --rate=<0.0-1.0> [--min-age=<sec>]
ic discovery rollback --source=<s> --since=<unix-ts>
ic discovery search --embedding=@<file> [--source=<s>] [--min-score=<f>] [--limit=N]
```

### Cost

```
ic cost reconcile <run_id> --billed-in=N --billed-out=N [--dispatch=<id>] [--source=<s>]
ic cost list <run_id> [--limit=N]
```

### Usage evidence

```
ic usage observe --record=<file>
ic usage list [--limit=N | --observation=ID]
ic usage validate --observation=<id> [--max-age=<seconds>]
```

`usage observe` accepts one regular JSON file no larger than 1 MiB. Its dedicated
parser rejects unknown, duplicate, missing-value, and positional arguments. The
record is a closed object with `id`, `provider`, `source`, `kind`, `status`,
nullable `reason`, separate capture/source/interval/reset times, raw nullable
`counters`, `identity`, `execution_refs`, sanitized `payload_sha256`, and nullable
`supersedes`. Counter units and provider semantics are never combined; null and
zero remain distinct. JSON number spellings are retained exactly, including
integers above 2^53. Numbers are limited to 4096 characters and exponent magnitude
10000. Stable-ID retries must be byte-equivalent after canonical encoding;
equivalent spellings such as `1` and `1.0` conservatively conflict. Corrections
append compatible provider/source/kind successors.

Validation records structural validity, including strict canonical input and
digest verification, and defaults to a 3600-second freshness threshold. It scans every
`dispatches` and `sessions` row without project, provider, account, status, or
result-limit filters. Closed intervals use `COALESCE(dispatch.started_at,
dispatch.created_at)` and open NULL ends. Only verified `dispatches.id` or numeric
`sessions.id` bindings can move overlap into `bound_activity`; native strings do
not. Results never claim account, kernel, or external exclusivity:
`external_activity` is always `unknown`, and absent or invalid evidence yields an
explicit unknown classification. A complete valid scan with zero activity reports
`none-observed`; it still cannot establish account exclusivity.

### Interspect

```
ic interspect record --agent=<name> --type=<type> [--run=<id>] [--reason=<text>] [--context=<json>] [--session=<id>] [--project=<dir>]
ic interspect query [--agent=<name>] [--since=<id>] [--limit=N]
```

### Portfolio

```
ic run create --projects=<p1>,<p2> --goal=<text> [--max-dispatches=N]
ic portfolio dep add <id> --upstream=<path> --downstream=<path>
ic portfolio dep list <id>
ic portfolio dep remove <id> --upstream=<path> --downstream=<path>
ic portfolio relay <id> [--interval=2s]
ic portfolio order <id>                    Topological build order (deterministic)
ic portfolio status <id>                   Per-child readiness with blocked-by details
```

### Situation

Unified observation layer for OODARC loops.

```
ic situation snapshot                      JSON snapshot of all active runs, dispatches, events, queue depth
ic situation snapshot --run=<id>           Scoped to a specific run (includes budget)
ic situation snapshot --events=50          Control event history depth (default: 20)
```

### Config & Agency

```
ic config set <key> <value>                Set kernel config (global_max_dispatches, max_spawn_depth)
ic config get <key>                        Get kernel config value
ic config list [--verbose]                 List all kernel config values
ic agency load <stage|all> --run=<id> --spec-dir=<path>
ic agency validate <file> | --all --spec-dir=<path>
ic agency show <stage> --spec-dir=<path>
ic agency capabilities <run-id>
```

### Publish

```
ic publish <version>                      Bump to exact version and publish
ic publish --patch                        Auto-increment patch version
ic publish --minor                        Auto-increment minor version
ic publish --auto [--cwd=<d>]             Auto mode (hooks): patch, no prompts
ic publish --dry-run                      Show what would happen
ic publish --patch --scoped               Preserve unrelated local state and peer marketplace checkouts
ic publish init [--name=<name>]           Register plugin in marketplace
ic publish status [--all]                 Show publish state
ic publish doctor [--fix] [--json]        Detect/repair drift
ic publish clean [--dry-run]              Prune orphans, stale versions
```

**Pipeline phases:** discovery -> validation -> bump -> commit plugin -> push plugin -> update marketplace -> sync local -> sync agent-rig.json -> done. Each phase is tracked in SQLite for crash recovery. The agent-rig sync is best-effort: after marketplace sync, checks if the plugin is listed in `os/clavain/agent-rig.json` and adds it to the recommended tier if missing.

`--scoped` retains versioning, validation, plugin and canonical marketplace
commits/pushes, the selected plugin's cache, installed record, hook bridges and
release canary. It skips all peer marketplace synchronization/refresh, global
cache pruning (including orphans and dangling links), cross-repo rig updates and
diagram generation. Peer indexes intentionally remain unchanged and may report
version drift; the immediate release probe checks the canonical marketplace.
Canonical means the checkout selected by normal discovery; outside the monorepo
this can be the Claude Code checkout itself. Inspect the printed path.
No cleanup is scheduled for later. Combine with `--dry-run` to preview the scope.
The default publish pipeline is unchanged. This flag does not bypass dirty-worktree,
approval or release-artifact gates. Normal Git push protection still applies;
the flag does not impose a branch policy.

The canary records scoped mode and the resolved canonical checkout path.
`ic publish rollback <plugin>` honors that record even when invoked elsewhere:
it restores the selected plugin and canonical marketplace without syncing or
refreshing peers. If that checkout is gone, rollback fails instead of using a
peer. Unreadable canary state also fails closed; legacy records without scoped
mode retain the existing rollback behavior.

## Exit Codes

| Code | Meaning | Example |
|------|---------|---------|
| 0 | Success / allowed / found | `ic state get` returns payload |
| 1 | Expected negative result | `ic sentinel check` throttled, `ic coordination check` conflict |
| 2 | Unexpected error | Invalid JSON, DB corruption |
| 3 | Usage error | Missing required argument |

## Global Flags

- `--db=<path>` -- Database path (default: `.clavain/intercore.db`, auto-discovered)
- `--timeout=<dur>` -- SQLite busy timeout (default: 100ms)
- `--verbose` -- Verbose output
- `--json` -- JSON output (must appear before subcommand)
