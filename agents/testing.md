# Testing & Recovery

Test suites and recovery procedures for common failure modes.

## Testing

```bash
go test ./...                    # Unit tests
go test -race ./...              # Race detector
bash test-integration.sh         # Full CLI integration test
go test -tags icsched_test ./... # Scheduler runner tests (Linux)
```

### Scheduler runner build tag

`ic scheduler run` is compiled only under the `icsched_test` build tag. A
release build exits 2 with "scheduler runner not enabled in this build" and
touches nothing. The runner tests (`internal/scheduler/runner`, and the
`TestCLI*` tests in `cmd/ic`, which build the tagged binary) run only with
`-tags icsched_test`. Run them with a throwaway HOME, a working directory under
a temp dir and an explicit `--db`; never point a runner at
`~/.clavain/intercore.db`. The runner refuses a database under a live
`.clavain/` directory unless `IC_SCHEDULER_ALLOW_LIVE=1` is set.

The A2 and A7 tests use unprivileged user namespaces (`unshare -Urm`,
`unshare -Un`) and skip with a reason where those are unavailable.

## Recovery Procedures

### DB Corruption
```bash
ic health                        # Diagnose
cp .clavain/intercore.db.backup-<from>-<stamp>.db .clavain/intercore.db  # Restore a chosen backup (pick the newest by stamp)
ic health                        # Verify
```

### Schema Mismatch
```bash
ic version                       # Shows "schema: v<N>"
# If binary too old: upgrade intercore binary
# If DB too old: ic init (auto-migrates)
```

### Sentinel Stuck After Crash
```bash
ic sentinel reset <name> <scope_id>
```

### Lock Stuck After Crash
```bash
ic lock stale --older-than=5s
ic lock clean --older-than=5s    # Checks PID liveness before removal
```

### Coordination Lock Stuck
```bash
ic coordination list --active
ic coordination sweep            # Expire TTL-based locks
ic coordination release <id>     # Manual release
```
