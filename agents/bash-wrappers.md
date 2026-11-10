# Bash Wrappers (lib-intercore.sh)

Wrapper functions for use from bash hooks and scripts (`grep -c '^intercore_.*()' lib-intercore.sh` counts them). Key groups:

**State/Sentinel:** `intercore_available`, `intercore_remote_authority`, `intercore_state_set/get`, `intercore_sentinel_check/reset`, `intercore_check_or_die`, `intercore_cleanup_stale`

**Dispatch:** `intercore_dispatch_spawn/status/wait/list_active/kill/tokens`

**Run:** `intercore_run_current/phase/advance/skip/tokens/budget`, `intercore_run_agent_add`, `intercore_run_artifact_add`, `intercore_run_rollback/rollback_dry/code_rollback`

**Actions:** `intercore_run_action_add/list/update/delete`

**Gates:** `intercore_gate_check/override`

**Locks:** `intercore_lock_available/lock/unlock/lock_clean`

**Events:** `intercore_events_tail/tail_all/cursor_get/cursor_set/cursor_reset`

**Agency:** `intercore_agency_load/validate`

## Canonical copy and sync check

Clavain's `hooks/lib-intercore.sh` is the canonical copy: hooks source it, and behavior changes (for example the remote-authority guard and the three-state `intercore_state_get`) land there first. The `lib-intercore.sh` at this repo's root is a byte-identical mirror that `test-integration.sh` sources to exercise the wrappers against a real `ic` binary. To refresh the mirror:

```bash
cp "$CLAVAIN_DIR/hooks/lib-intercore.sh" lib-intercore.sh
```

`test-integration.sh` compares the two with `cmp` and fails when they differ. It finds Clavain through `$CLAVAIN_DIR`, or the sibling `../../os/Clavain` and `../Clavain` checkouts. When none is found, the sync check and the agency-spec checks print `SKIP` and are counted in the final line; set `IC_REQUIRE_CLAVAIN=1` to make a missing checkout a failure instead.
