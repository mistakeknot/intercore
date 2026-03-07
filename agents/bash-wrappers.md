# Bash Wrappers (lib-intercore.sh)

45 wrapper functions for use from bash hooks and scripts. Key groups:

**State/Sentinel:** `intercore_available`, `intercore_state_set/get`, `intercore_sentinel_check/reset`, `intercore_check_or_die`, `intercore_cleanup_stale`

**Dispatch:** `intercore_dispatch_spawn/status/wait/list_active/kill/tokens`

**Run:** `intercore_run_current/phase/advance/skip/tokens/budget`, `intercore_run_agent_add`, `intercore_run_artifact_add`, `intercore_run_rollback/rollback_dry/code_rollback`

**Actions:** `intercore_run_action_add/list/update/delete`

**Gates:** `intercore_gate_check/override`

**Locks:** `intercore_lock_available/lock/unlock/lock_clean`

**Events:** `intercore_events_tail/tail_all/cursor_get/cursor_set/cursor_reset`

**Agency:** `intercore_agency_load/validate`
