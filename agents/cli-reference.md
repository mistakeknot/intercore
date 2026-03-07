# CLI Reference

Complete `ic` command reference, flags, and exit codes.

## Architecture

```
cmd/ic/          CLI entry point + 20 subcommand files (run, dispatch, gate, lock, events, coordination, scheduler, lane, discovery, portfolio, cost, interspect, agency, action, config, publish, route, landed, session, situation)
internal/        30 packages — key ones: db/, state/, dispatch/, phase/, event/, coordination/, scheduler/, lane/, discovery/, portfolio/, budget/, lock/, agency/, audit/, redaction/, publish/
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
ic dispatch tokens <id> --set --in=N --out=N [--cache=N]   Update token counts
ic dispatch prune --older-than=<dur>       Remove old terminal dispatches
```

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
ic events cursor list                      List consumer cursors
ic events cursor reset <consumer>          Reset a consumer cursor
```

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
ic publish init [--name=<name>]           Register plugin in marketplace
ic publish status [--all]                 Show publish state
ic publish doctor [--fix] [--json]        Detect/repair drift
ic publish clean [--dry-run]              Prune orphans, stale versions
```

**Pipeline phases:** discovery -> validation -> bump -> commit plugin -> push plugin -> update marketplace -> sync local -> sync agent-rig.json -> done. Each phase is tracked in SQLite for crash recovery. The agent-rig sync is best-effort: after marketplace sync, checks if the plugin is listed in `os/clavain/agent-rig.json` and adds it to the recommended tier if missing.

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
