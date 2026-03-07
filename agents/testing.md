# Testing & Recovery

Test suites and recovery procedures for common failure modes.

## Testing

```bash
go test ./...                    # Unit tests (~529 test functions across 23 packages)
go test -race ./...              # Race detector
bash test-integration.sh         # Full CLI integration test (1320-line bash suite)
```

## Recovery Procedures

### DB Corruption
```bash
ic health                        # Diagnose
cp .clavain/intercore.db.backup-* .clavain/intercore.db  # Restore latest backup
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
