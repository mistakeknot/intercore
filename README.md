# intercore

Orchestration kernel for autonomous software development: the durable system of record for runs, phases, gates, dispatches, events, and token budgets.

## What this does

When agents work through a multi-phase development lifecycle (brainstorm, plan, execute, review, ship), something needs to track where they are, what they've done, and whether they're allowed to proceed. intercore is that something. It's a Go CLI (`ic`) backed by a single SQLite WAL database that provides mechanism without policy: it doesn't know what "brainstorm" means, only that a phase transition happened and needs recording.

In the three-layer architecture, intercore is Layer 1. Clavain (Layer 2) and companion plugins (Layer 3) call `ic` for all state operations. If the host platform changes, the kernel and all its data survive untouched.

## Run

```bash
go build -o ic ./cmd/ic
./ic run create --project=. --goal="Implement feature X"
```

Or use the prebuilt binary if available on `PATH`.

## Key commands

| Command | What it does |
|---------|-------------|
| `ic run create` | Start a new run with phase chain and optional budget |
| `ic run advance <id>` | Advance to next phase (evaluates gates) |
| `ic dispatch spawn` | Spawn an agent with prompt, project, and output path |
| `ic dispatch wait <id>` | Block until dispatch completes |
| `ic events tail <id>` | Stream events for a run (supports `-f` follow mode) |
| `ic gate check <id>` | Dry-run gate evaluation |
| `ic lock acquire <name>` | Filesystem mutex (no SQLite, works even if DB is broken) |
| `ic portfolio relay` | Cross-project event relay for multi-project coordination |
| `ic discovery submit` | Submit a finding to the discovery pipeline |
| `ic cost reconcile <id>` | Compare billed vs self-reported tokens |

## Architecture

- **Database:** `.clavain/intercore.db` (SQLite WAL, pure Go driver, auto-discovered by walking up from CWD)
- **CLI only:** No Go library API: all consumers shell out to `ic`
- **Event-driven:** Phase transitions fire events; handlers auto-spawn agents and execute hooks
- **Optimistic concurrency:** Phase advances use `WHERE phase = ?` to detect races
- **Portfolio orchestration:** Parent runs coordinate child runs via event relay + dependency DAG

## Testing

```bash
go test ./...               # Unit tests
go test -race ./...         # With race detector
bash test-integration.sh    # Integration tests (builds ic, creates temp DB)
```

## License

MIT
