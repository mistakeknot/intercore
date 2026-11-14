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

`ic scheduler run` and `ic scheduler runner` are compiled only under the
`icsched_test` build tag. A release build exits 2 with "scheduler runner not
enabled in this build" and touches nothing. The runner tests (`internal/scheduler/runner`, and the
`TestCLI*` tests in `cmd/ic`, which build the tagged binary) run only with
`-tags icsched_test`. Run them with a throwaway HOME, a working directory under
a temp dir and an explicit `--db`; never point a runner at
`~/.clavain/intercore.db`. The runner refuses a database under a live
`.clavain/` directory unless `IC_SCHEDULER_ALLOW_LIVE=1` is set.

The claim and launch tests use a fake executor
(`internal/scheduler/runner/testdata/fake-dispatch.sh`) and in-package seams
(`hooks` in `options.go`) so races are forced, not slept through; each one has
a control that lifts the condition it proves. The in-process tests count
executor runs from `$FAKE_LOG` and from the dispatch ledger. The CLI tests
(`scheduler_run_launch_test.go`) run the tagged binary under `strace -f -e
trace=execve` and count `execve` calls from outside the process; they skip with
the reason when strace cannot trace here. Run concurrency tests with `-race
-count=50`.

The A2 and A7 tests use unprivileged user namespaces (`unshare -Urm`,
`unshare -Un`) and skip with a reason where those are unavailable.

## Recovery Procedures

### Migration backups

Migration attempts on an older database create a verified, WAL-inclusive
snapshot before changing the schema. Already-current databases are skipped.
Set `IC_BACKUP_DIR` to place snapshots in another directory (created with mode
0700 when absent); the default is beside the database. New snapshot names are
`<db>.backup-<identity>-<schema>-YYYYMMDD-HHMMSS[-<collision>].db`, where identity
is a hash of the cleaned absolute database path. Databases with the same filename
can share a backup directory without pruning each other's snapshots.
`IC_BACKUP_KEEP` is a positive integer, defaulting to 5; an invalid value blocks
migration. After a verified snapshot is published, retention keeps the newest
snapshots by modification time, removing only regular files matching that
identity. Legacy `backup-YYYYMMDD-HHMMSS` and
`backup-<schema>-YYYYMMDD-HHMMSS[-<collision>].db` names participate only when
backups are beside the database. Symlinks, directories, pending snapshots and
other filenames are excluded. Changing the directory or database path does not
move or prune snapshots in the previous location or identity. Failed snapshots
do not trigger retention. Publication uses hard links where available, with an
exclusive-create, verified-copy fallback. Retention errors are logged and do not
block migration after a verified snapshot.

### DB Corruption
```bash
ic health                        # Diagnose
cp .clavain/intercore.db.backup-<identity>-<from>-<stamp>.db .clavain/intercore.db  # Restore a chosen backup (pick the newest by stamp)
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
