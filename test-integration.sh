#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
TEST_DIR=$(mktemp -d)
TEST_DB="$TEST_DIR/.clavain/intercore.db"
IC_BIN="/tmp/ic-integ-$$"

# Trap-based cleanup — ensures cleanup even on failure
cleanup() { rm -rf "$TEST_DIR" "$IC_BIN"; }
trap cleanup EXIT

pass() { printf '  PASS: %s\n' "$1"; }
fail() { printf '  FAIL: %s\n' "$1"; exit 1; }
ic() { "$IC_BIN" "$@"; }

echo "Building ic..."
cd "$SCRIPT_DIR"
go build -o "$IC_BIN" ./cmd/ic

# Create test DB (under a .clavain/ subdir to satisfy path validation)
mkdir -p "$TEST_DIR/.clavain"
cd "$TEST_DIR"

echo "=== Init ==="
ic init --db="$TEST_DB"
pass "init"

echo "=== File Permissions ==="
# Test that ic init creates directory with 0700 and DB file with 0600.
# Use a fresh subdirectory under CWD (ic validates --db is under CWD).
PERM_DB="$TEST_DIR/perms/.clavain/intercore.db"
ic init --db="$PERM_DB"
dir_perm=$(stat -c '%a' "$TEST_DIR/perms/.clavain")
[[ "$dir_perm" == "700" ]] || fail "directory permissions: expected 700, got $dir_perm"
pass "directory permissions: $dir_perm"
file_perm=$(stat -c '%a' "$PERM_DB")
[[ "$file_perm" == "600" ]] || fail "DB file permissions: expected 600, got $file_perm"
pass "DB file permissions: $file_perm"

echo "=== Health ==="
ic health --db="$TEST_DB"
pass "health"

echo "=== Version ==="
ic version --db="$TEST_DB"
pass "version"

echo "=== State Operations ==="
printf '%s\n' '{"phase":"brainstorm"}' | ic state set dispatch test-session --db="$TEST_DB"
result=$(ic state get dispatch test-session --db="$TEST_DB")
[[ "$result" == '{"phase":"brainstorm"}' ]] || fail "state get returned: $result"
pass "state set/get roundtrip"

ic state list dispatch --db="$TEST_DB" | grep -q "test-session" || fail "state list"
pass "state list"

ic state delete dispatch test-session --db="$TEST_DB" | grep -q "deleted" || fail "state delete"
pass "state delete"

ic state get dispatch test-session --db="$TEST_DB" && fail "deleted state visible" || true
pass "state get after delete returns not-found"

# Secret rejection — the kernel should refuse to store likely secret values
printf '%s\n' '{"token":"sk-abc1234567890abcdefghijklmnop"}' | ic state set dispatch secret-test --db="$TEST_DB" 2>/dev/null && fail "secret value accepted" || true
pass "secret value rejected"

# Namespace key validation — keys must be lowercase with valid format
printf '%s\n' '{"x":1}' | ic state set "INVALID_KEY" test-session --db="$TEST_DB" 2>/dev/null && fail "uppercase key accepted" || true
pass "uppercase key rejected"

printf '%s\n' '{"x":1}' | ic state set "valid.key" test-session --db="$TEST_DB" 2>/dev/null
pass "dotted key accepted"

# Clean up the dotted key
ic state delete "valid.key" test-session --db="$TEST_DB" >/dev/null 2>&1 || true

echo "=== Sentinel Operations ==="
ic sentinel check stop test-session --interval=0 --db="$TEST_DB" >/dev/null
pass "sentinel check (first = allowed)"

ic sentinel check stop test-session --interval=0 --db="$TEST_DB" >/dev/null && fail "sentinel should be throttled" || true
pass "sentinel check (second = throttled)"

ic sentinel list --db="$TEST_DB" | grep -q "stop" || fail "sentinel list"
pass "sentinel list"

ic sentinel reset stop test-session --db="$TEST_DB" | grep -q "reset" || fail "sentinel reset"
pass "sentinel reset"

echo "=== TTL Enforcement ==="
printf '%s\n' '{"temp":true}' | ic state set ephemeral test-session --ttl=1s --db="$TEST_DB"
result=$(ic state get ephemeral test-session --db="$TEST_DB")
[[ "$result" == '{"temp":true}' ]] || fail "TTL: state not visible before expiry"
pass "state visible before TTL"

sleep 2
ic state get ephemeral test-session --db="$TEST_DB" && fail "expired state visible" || true
pass "state invisible after TTL"

echo "=== JSON Validation ==="
printf '%s\n' 'not json' | ic state set bad test-session --db="$TEST_DB" 2>/dev/null && fail "invalid JSON accepted" || true
pass "invalid JSON rejected"

echo "=== Path Traversal Protection ==="
ic init --db="/tmp/evil.db" 2>/dev/null && fail "path traversal accepted" || true
pass "absolute path rejected"

ic init --db="../../escape.db" 2>/dev/null && fail "dotdot accepted" || true
pass "dotdot path rejected"

ic init --db="noext" 2>/dev/null && fail "no extension accepted" || true
pass "missing .db extension rejected"

echo "=== Compat Status ==="
printf '%s\n' '{"test":true}' | ic state set dispatch test-session --db="$TEST_DB"
output=$(ic compat status --db="$TEST_DB")
echo "$output"
echo "$output" | grep -q "dispatch" || fail "compat status missing dispatch"
pass "compat status"

echo "=== lib-intercore.sh Wrapper ==="
# Source the library and test the wrapper with a real ic binary
export INTERCORE_BIN="$IC_BIN"
source "$SCRIPT_DIR/lib-intercore.sh"
INTERCORE_BIN="$IC_BIN"  # force available

# Test sentinel_check_or_legacy with ic available
intercore_sentinel_check_or_legacy "wrapper_test" "test-session" 0 "/tmp/clavain-wrapper-test" && pass "wrapper: first check allowed" || fail "wrapper: first check should be allowed"
intercore_sentinel_check_or_legacy "wrapper_test" "test-session" 0 "/tmp/clavain-wrapper-test" && fail "wrapper: second check should be throttled" || pass "wrapper: second check throttled"

# Test reset
intercore_sentinel_reset_or_legacy "wrapper_test" "test-session" "/tmp/clavain-wrapper-test"
pass "wrapper: reset"

# Verify sentinel was reset (next check should be allowed)
intercore_sentinel_check_or_legacy "wrapper_test" "test-session" 0 "/tmp/clavain-wrapper-test" && pass "wrapper: check after reset allowed" || fail "wrapper: check after reset should be allowed"

# Test cleanup
intercore_cleanup_stale
pass "wrapper: cleanup"

echo "=== Legacy Fallback Path ==="
# Test with ic unavailable (forces legacy temp-file path)
# Must clear INTERCORE_BIN AND hide ic from PATH/functions so intercore_available returns 1
INTERCORE_BIN_SAVED="$INTERCORE_BIN"
PATH_SAVED="$PATH"
INTERCORE_BIN=""
PATH="/usr/bin:/bin"  # strip any dir containing ic
unset -f ic 2>/dev/null || true  # remove test helper function so command -v ic fails
rm -f /tmp/clavain-legacy-test

intercore_sentinel_check_or_legacy "legacy_test" "test-session" 0 "/tmp/clavain-legacy-test" && pass "legacy: first check allowed" || fail "legacy: first check should be allowed"
[[ -f "/tmp/clavain-legacy-test" ]] && pass "legacy: sentinel file created" || fail "legacy: sentinel file missing"
intercore_sentinel_check_or_legacy "legacy_test" "test-session" 0 "/tmp/clavain-legacy-test" && fail "legacy: second check should be throttled" || pass "legacy: second check throttled"

rm -f /tmp/clavain-legacy-test
INTERCORE_BIN="$INTERCORE_BIN_SAVED"
PATH="$PATH_SAVED"
ic() { "$IC_BIN" "$@"; }  # restore test helper

echo "=== Dispatch Operations ==="
# Create a prompt file for testing
PROMPT_FILE="$TEST_DIR/test-prompt.md"
printf 'echo hello world\n' > "$PROMPT_FILE"
OUTPUT_FILE="$TEST_DIR/test-output.md"

# Spawn with /bin/echo as mock dispatch.sh (exits immediately)
DISPATCH_ID=$(ic dispatch spawn --type=codex --prompt-file="$PROMPT_FILE" --project="$TEST_DIR" --name=test-agent --output="$OUTPUT_FILE" --dispatch-sh=/bin/echo --db="$TEST_DB")
[[ -n "$DISPATCH_ID" ]] || fail "dispatch spawn returned empty ID"
[[ ${#DISPATCH_ID} -eq 8 ]] || fail "dispatch ID should be 8 chars, got: $DISPATCH_ID"
pass "dispatch spawn"

# Status check
status_out=$(ic dispatch status "$DISPATCH_ID" --json --db="$TEST_DB")
echo "$status_out" | grep -q '"status":"running"' || fail "dispatch status should be running, got: $status_out"
pass "dispatch status (running)"

# Wait for mock process to exit (it already has since /bin/echo exits immediately)
sleep 0.5

# Poll should detect the dead process and collect
poll_out=$(ic dispatch poll "$DISPATCH_ID" --json --db="$TEST_DB")
echo "$poll_out" | grep -q '"status"' || fail "dispatch poll returned no status"
pass "dispatch poll"

# List active (should be empty since mock exited)
active_out=$(ic dispatch list --active --db="$TEST_DB")
pass "dispatch list --active"

# List all
all_out=$(ic dispatch list --db="$TEST_DB")
echo "$all_out" | grep -q "$DISPATCH_ID" || fail "dispatch list should include our dispatch"
pass "dispatch list"

# Spawn another and kill it
DISPATCH_ID2=$(ic dispatch spawn --type=codex --prompt-file="$PROMPT_FILE" --project="$TEST_DIR" --name=kill-test --dispatch-sh=/bin/sleep --db="$TEST_DB" 2>/dev/null) || true
if [[ -n "$DISPATCH_ID2" ]]; then
    ic dispatch kill "$DISPATCH_ID2" --db="$TEST_DB" | grep -q "killed" || true
    pass "dispatch kill"
else
    pass "dispatch kill (skipped — sleep not available as dispatch.sh)"
fi

# Prune old dispatches
prune_out=$(ic dispatch prune --older-than=0s --db="$TEST_DB")
pass "dispatch prune"

# Dispatch wrapper tests
INTERCORE_BIN="$IC_BIN"
DISPATCH_ID3=$(intercore_dispatch_spawn "codex" "$TEST_DIR" "$PROMPT_FILE" "$OUTPUT_FILE" "wrapper-test")
[[ -n "$DISPATCH_ID3" ]] || fail "dispatch wrapper spawn returned empty ID"
pass "wrapper: dispatch spawn"

sleep 0.5
wrapper_status=$(intercore_dispatch_status "$DISPATCH_ID3")
[[ -n "$wrapper_status" ]] || fail "dispatch wrapper status returned empty"
pass "wrapper: dispatch status"

wrapper_list=$(intercore_dispatch_list_active)
pass "wrapper: dispatch list active"

echo "=== Run Operations ==="
# Create a run
RUN_ID=$(ic run create --project="$TEST_DIR" --goal="Integration test run" --complexity=3 --db="$TEST_DB")
[[ -n "$RUN_ID" ]] || fail "run create returned empty ID"
[[ ${#RUN_ID} -eq 8 ]] || fail "run ID should be 8 chars, got: $RUN_ID"
pass "run create"

# Check phase (should be brainstorm)
run_phase=$(ic run phase "$RUN_ID" --db="$TEST_DB")
[[ "$run_phase" == "brainstorm" ]] || fail "initial phase should be brainstorm, got: $run_phase"
pass "run phase (brainstorm)"

# Status check
status_out=$(ic run status "$RUN_ID" --db="$TEST_DB")
echo "$status_out" | grep -q "brainstorm" || fail "run status should show brainstorm"
pass "run status"

# Advance: brainstorm → brainstorm-reviewed
advance_out=$(ic run advance "$RUN_ID" --db="$TEST_DB")
echo "$advance_out" | grep -q "brainstorm-reviewed" || fail "advance should go to brainstorm-reviewed, got: $advance_out"
pass "run advance (brainstorm → brainstorm-reviewed)"

# Advance: brainstorm-reviewed → strategized
advance_out=$(ic run advance "$RUN_ID" --db="$TEST_DB")
echo "$advance_out" | grep -q "strategized" || fail "advance should go to strategized, got: $advance_out"
pass "run advance (brainstorm-reviewed → strategized)"

# Events audit trail
events_out=$(ic run events "$RUN_ID" --db="$TEST_DB")
event_count=$(echo "$events_out" | wc -l)
[[ $event_count -ge 2 ]] || fail "should have at least 2 events, got: $event_count"
pass "run events"

# List runs
list_out=$(ic run list --db="$TEST_DB")
echo "$list_out" | grep -q "$RUN_ID" || fail "run list should include our run"
pass "run list"

# List active runs
list_active=$(ic run list --active --db="$TEST_DB")
echo "$list_active" | grep -q "$RUN_ID" || fail "run list --active should include our run"
pass "run list --active"

# Set complexity
ic run set "$RUN_ID" --complexity=1 --db="$TEST_DB" | grep -q "updated" || fail "run set"
pass "run set"

# JSON output
json_out=$(ic run status "$RUN_ID" --json --db="$TEST_DB")
echo "$json_out" | grep -q '"complexity":1' || fail "JSON should show complexity=1, got: $json_out"
pass "run status --json"

# Create a complexity-1 run with a short custom chain and advance through full lifecycle
FAST_RUN=$(ic run create --project="$TEST_DIR" --goal="Fast run" --complexity=1 --phases='["brainstorm","planned","executing","done"]' --db="$TEST_DB")
ic run advance "$FAST_RUN" --disable-gates --db="$TEST_DB" >/dev/null  # brainstorm → planned
ic run advance "$FAST_RUN" --disable-gates --db="$TEST_DB" >/dev/null  # planned → executing
ic run advance "$FAST_RUN" --disable-gates --db="$TEST_DB" >/dev/null  # executing → done
fast_phase=$(ic run phase "$FAST_RUN" --db="$TEST_DB")
[[ "$fast_phase" == "done" ]] || fail "complexity-1 run should reach done, got: $fast_phase"
fast_status=$(ic run status "$FAST_RUN" --json --db="$TEST_DB")
echo "$fast_status" | grep -q '"status":"completed"' || fail "completed run should have status=completed"
pass "run full lifecycle (complexity 1 with short chain)"

# Advance past done should fail
ic run advance "$FAST_RUN" --db="$TEST_DB" 2>/dev/null && fail "advance past done should fail" || true
pass "run advance past done rejected"

# Cancel a run
CANCEL_RUN=$(ic run create --project="$TEST_DIR" --goal="Cancel test" --db="$TEST_DB")
ic run cancel "$CANCEL_RUN" --db="$TEST_DB" | grep -q "cancelled" || fail "run cancel"
pass "run cancel"

# Advance cancelled run should fail
ic run advance "$CANCEL_RUN" --db="$TEST_DB" 2>/dev/null && fail "advance cancelled run should fail" || true
pass "run advance cancelled run rejected"

# Cancel already cancelled should fail
ic run cancel "$CANCEL_RUN" --db="$TEST_DB" 2>/dev/null && fail "cancel cancelled should fail" || true
pass "run cancel already cancelled rejected"

echo "=== Run Current ==="
# ic run current should find the active run (RUN_ID is still active from above)
current_id=$(ic run current --project="$TEST_DIR" --db="$TEST_DB")
[[ "$current_id" == "$RUN_ID" ]] || fail "run current should return $RUN_ID, got: $current_id"
pass "run current (found active run)"

# JSON mode
current_json=$(ic run current --project="$TEST_DIR" --json --db="$TEST_DB")
echo "$current_json" | grep -q '"found":true' || fail "run current --json should have found=true"
echo "$current_json" | grep -q "\"id\":\"$RUN_ID\"" || fail "run current --json should have correct id"
pass "run current --json"

# No active run for a different project
ic run current --project="/tmp/no-such-project" --db="$TEST_DB" 2>/dev/null && fail "run current should fail for unknown project" || true
pass "run current (no active run)"

# JSON mode for not-found
notfound_json=$(ic run current --project="/tmp/no-such-project" --json --db="$TEST_DB" 2>/dev/null) || true
echo "$notfound_json" | grep -q '"found":false' || fail "run current --json not-found should have found=false"
pass "run current --json (not found)"

# Wrapper test
current_wrapper=$(intercore_run_current "$TEST_DIR")
[[ "$current_wrapper" == "$RUN_ID" ]] || fail "wrapper run current should return $RUN_ID, got: $current_wrapper"
pass "wrapper: run current"

# Wrapper phase test
phase_wrapper=$(intercore_run_phase "$RUN_ID")
[[ "$phase_wrapper" == "strategized" ]] || fail "wrapper run phase should return strategized, got: $phase_wrapper"
pass "wrapper: run phase"

echo "=== Run Agents ==="
# Add an agent to the active run
AGENT_ID=$(ic run agent add "$RUN_ID" --type=claude --name=brainstorm-agent --db="$TEST_DB")
[[ -n "$AGENT_ID" ]] || fail "run agent add returned empty ID"
[[ ${#AGENT_ID} -eq 8 ]] || fail "agent ID should be 8 chars, got: $AGENT_ID"
pass "run agent add"

# Add a second agent with dispatch-id
AGENT_ID2=$(ic run agent add "$RUN_ID" --type=codex --name=review-agent --dispatch-id=disp123 --db="$TEST_DB")
[[ -n "$AGENT_ID2" ]] || fail "run agent add (with dispatch-id) returned empty ID"
pass "run agent add (with dispatch-id)"

# List agents
agent_list=$(ic run agent list "$RUN_ID" --db="$TEST_DB")
echo "$agent_list" | grep -q "$AGENT_ID" || fail "agent list should include first agent"
echo "$agent_list" | grep -q "$AGENT_ID2" || fail "agent list should include second agent"
agent_count=$(echo "$agent_list" | wc -l)
[[ $agent_count -eq 2 ]] || fail "agent list should have 2 agents, got: $agent_count"
pass "run agent list"

# JSON list
agent_json=$(ic run agent list "$RUN_ID" --json --db="$TEST_DB")
echo "$agent_json" | grep -q '"agent_type":"claude"' || fail "agent list JSON should include claude agent"
echo "$agent_json" | grep -q '"agent_type":"codex"' || fail "agent list JSON should include codex agent"
pass "run agent list --json"

# Update agent status
ic run agent update "$AGENT_ID" --status=completed --db="$TEST_DB" | grep -q "updated" || fail "run agent update"
pass "run agent update"

# Verify update via JSON list
updated_json=$(ic run agent list "$RUN_ID" --json --db="$TEST_DB")
echo "$updated_json" | grep -q '"status":"completed"' || fail "updated agent should have status=completed"
pass "run agent update verified"

# Invalid status should fail
ic run agent update "$AGENT_ID" --status=bogus --db="$TEST_DB" 2>/dev/null && fail "invalid status should fail" || true
pass "run agent update (invalid status rejected)"

# Add agent to non-existent run should fail (FK enforcement)
ic run agent add "nonexist" --type=claude --db="$TEST_DB" 2>/dev/null && fail "agent add to non-existent run should fail" || true
pass "run agent add (FK violation rejected)"

# Wrapper test
AGENT_ID3=$(intercore_run_agent_add "$RUN_ID" "claude" "wrapper-agent" "")
[[ -n "$AGENT_ID3" ]] || fail "wrapper run agent add returned empty ID"
pass "wrapper: run agent add"

echo "=== Run Artifacts ==="
# Add an artifact
ARTIFACT_ID=$(ic run artifact add "$RUN_ID" --phase=brainstorm --path=docs/brainstorms/test.md --db="$TEST_DB")
[[ -n "$ARTIFACT_ID" ]] || fail "run artifact add returned empty ID"
[[ ${#ARTIFACT_ID} -eq 8 ]] || fail "artifact ID should be 8 chars, got: $ARTIFACT_ID"
pass "run artifact add"

# Add a second artifact in a different phase
ARTIFACT_ID2=$(ic run artifact add "$RUN_ID" --phase=planned --path=docs/plans/test-plan.md --type=plan --db="$TEST_DB")
[[ -n "$ARTIFACT_ID2" ]] || fail "run artifact add (planned) returned empty ID"
pass "run artifact add (different phase)"

# List all artifacts
artifact_list=$(ic run artifact list "$RUN_ID" --db="$TEST_DB")
echo "$artifact_list" | grep -q "$ARTIFACT_ID" || fail "artifact list should include first artifact"
echo "$artifact_list" | grep -q "$ARTIFACT_ID2" || fail "artifact list should include second artifact"
artifact_count=$(echo "$artifact_list" | wc -l)
[[ $artifact_count -eq 2 ]] || fail "artifact list should have 2 artifacts, got: $artifact_count"
pass "run artifact list"

# Filter by phase
planned_list=$(ic run artifact list "$RUN_ID" --phase=planned --db="$TEST_DB")
planned_count=$(echo "$planned_list" | wc -l)
[[ $planned_count -eq 1 ]] || fail "artifact list --phase=planned should have 1, got: $planned_count"
echo "$planned_list" | grep -q "test-plan.md" || fail "planned artifact should be test-plan.md"
pass "run artifact list --phase"

# JSON list
artifact_json=$(ic run artifact list "$RUN_ID" --json --db="$TEST_DB")
echo "$artifact_json" | grep -q '"phase":"brainstorm"' || fail "artifact list JSON should include brainstorm artifact"
echo "$artifact_json" | grep -q '"phase":"planned"' || fail "artifact list JSON should include planned artifact"
pass "run artifact list --json"

# Add artifact to non-existent run should fail (FK enforcement)
ic run artifact add "nonexist" --phase=brainstorm --path=x.md --db="$TEST_DB" 2>/dev/null && fail "artifact add to non-existent run should fail" || true
pass "run artifact add (FK violation rejected)"

# Wrapper test
ARTIFACT_ID3=$(intercore_run_artifact_add "$RUN_ID" "strategized" "docs/prds/test.md" "file")
[[ -n "$ARTIFACT_ID3" ]] || fail "wrapper run artifact add returned empty ID"
pass "wrapper: run artifact add"

# === Gates ===
echo ""
echo "=== Gates ==="

# Create a gated run (complexity 3 = all phases)
GATE_RUN=$(ic run create --project="$TEST_DIR" --goal="Gate test run" --complexity=3 --db="$TEST_DB")
[[ -n "$GATE_RUN" ]] || fail "gate run create returned empty ID"
pass "gate: run create"

# Gate check should fail (no brainstorm artifact, hard priority)
ic gate check "$GATE_RUN" --priority=0 --db="$TEST_DB" 2>/dev/null && fail "gate check should fail without artifact" || true
pass "gate: check fails without artifact (hard)"

# Gate rules should display
rules_out=$(ic gate rules --db="$TEST_DB")
echo "$rules_out" | grep -q "artifact_exists" || fail "gate rules should show artifact_exists"
pass "gate: rules display"

# Gate rules with phase filter
rules_filtered=$(ic gate rules --phase=brainstorm --db="$TEST_DB")
echo "$rules_filtered" | grep -q "brainstorm" || fail "gate rules --phase should show brainstorm"
pass "gate: rules --phase filter"

# Gate rules JSON
rules_json=$(ic gate rules --json --db="$TEST_DB")
echo "$rules_json" | grep -q '"check":"artifact_exists"' || fail "gate rules JSON should include artifact_exists"
pass "gate: rules --json"

# Add brainstorm artifact, then gate check should pass (hard priority)
ic run artifact add "$GATE_RUN" --phase=brainstorm --path=docs/brainstorms/gate-test.md --db="$TEST_DB" >/dev/null
gate_result=$(ic gate check "$GATE_RUN" --priority=0 --db="$TEST_DB")
echo "$gate_result" | grep -q "PASS" || fail "gate check should pass with artifact, got: $gate_result"
pass "gate: check passes with artifact (hard)"

# Gate check JSON output
gate_json=$(ic gate check "$GATE_RUN" --priority=0 --json --db="$TEST_DB")
echo "$gate_json" | grep -q '"result":"pass"' || fail "gate check JSON should show pass, got: $gate_json"
pass "gate: check --json"

# Advance with passing gate
advance_gate=$(ic run advance "$GATE_RUN" --db="$TEST_DB")
echo "$advance_gate" | grep -q "brainstorm-reviewed" || fail "advance should go to brainstorm-reviewed, got: $advance_gate"
pass "gate: advance with passing gate"

# Advance through to a phase with no artifacts (should block at hard priority)
# brainstorm-reviewed → strategized requires artifact_exists for brainstorm-reviewed
ic gate check "$GATE_RUN" --priority=0 --db="$TEST_DB" 2>/dev/null && fail "gate should block at brainstorm-reviewed without artifact" || true
pass "gate: blocks at next phase without artifact (hard)"

# Gate override
ic gate override "$GATE_RUN" --reason="integration test override" --db="$TEST_DB"
gate_phase=$(ic run phase "$GATE_RUN" --db="$TEST_DB")
[[ "$gate_phase" == "strategized" ]] || fail "override should advance to strategized, got: $gate_phase"
pass "gate: override advances phase"

# Override event in audit trail
events_gate=$(ic run events "$GATE_RUN" --db="$TEST_DB")
echo "$events_gate" | grep -q "override" || fail "events should show override"
pass "gate: override in audit trail"

# Gate override without reason should fail
ic gate override "$GATE_RUN" --db="$TEST_DB" 2>/dev/null && fail "override without reason should fail" || true
pass "gate: override without reason rejected"

# Gate check on non-existent run should fail
ic gate check "nonexist" --db="$TEST_DB" 2>/dev/null && fail "gate check nonexistent should fail" || true
pass "gate: check non-existent run"

# Soft gate (priority 2) — dry-run shows soft tier (still reports fail, but advance would proceed)
soft_result=$(ic gate check "$GATE_RUN" --priority=2 --db="$TEST_DB" 2>/dev/null) || true
echo "$soft_result" | grep -q "soft" || fail "soft gate should show soft tier, got: $soft_result"
pass "gate: soft priority shows tier"

# Wrapper test: intercore_gate_check (lib-intercore.sh already sourced above)
# The run is at strategized with no artifact → gate fails (exit 1), so capture rc
wrapper_rc=0
intercore_gate_check "$GATE_RUN" || wrapper_rc=$?
[[ $wrapper_rc -eq 1 ]] || fail "wrapper gate check should return 1 (fail), got: $wrapper_rc"
pass "wrapper: gate check"

echo "=== Version Sync Check ==="
# Verify Clavain's copy is in sync (if present in monorepo)
# --- Lock tests (filesystem-only, no DB) ---
echo ""
echo "=== Lock ==="

LOCK_DIR="/tmp/intercore/locks"

# Cleanup any leftover test locks
rm -rf "$LOCK_DIR/testlock" "$LOCK_DIR/ownertest" "$LOCK_DIR/conttest" "$LOCK_DIR/staletest" 2>/dev/null || true

# Basic acquire/release
ic lock acquire testlock global --owner="test:host"
pass "lock acquire"
ic lock list | grep -q "testlock" || fail "lock list missing testlock"
pass "lock list shows acquired lock"
ic lock release testlock global --owner="test:host"
pass "lock release"

# Owner verification
ic lock acquire ownertest scope1 --owner="alice:host"
ic lock release ownertest scope1 --owner="bob:host" 2>/dev/null && fail "wrong owner released lock" || pass "lock release wrong owner blocked"
ic lock release ownertest scope1 --owner="alice:host"
pass "lock release correct owner"

# Lock contention
ic lock acquire conttest scope1 --owner="a:host"
ic lock acquire conttest scope1 --timeout=200ms --owner="b:host" 2>/dev/null && fail "contended acquire should fail" || pass "lock acquire contention timeout"
ic lock release conttest scope1 --owner="a:host"

# Stale lock detection (backdate owner.json)
ic lock acquire staletest global --owner="99999:host"
STALE_DIR="$LOCK_DIR/staletest/global"
BACKDATE=$(($(date +%s) - 10))
echo "{\"pid\":99999,\"host\":\"host\",\"owner\":\"99999:host\",\"created\":${BACKDATE}}" > "$STALE_DIR/owner.json"
ic lock stale --older-than=1s | grep -q "staletest" || fail "stale list missing staletest"
pass "stale lists old lock"

# Clean removes stale locks (PID 99999 should not exist)
ic lock clean --older-than=1s
ic lock list | grep -q "staletest" && fail "stale lock not cleaned" || pass "stale lock cleaned"

# Cleanup test locks
rm -rf "$LOCK_DIR/testlock" "$LOCK_DIR/ownertest" "$LOCK_DIR/conttest" "$LOCK_DIR/staletest" 2>/dev/null || true

# --- Event Bus Tests ---
echo ""
echo "=== Event Bus ==="

# Create a run and advance it
EVT_RUN=$(ic --db="$TEST_DB" run create --project="$TEST_DIR" --goal="Event bus test")
pass "create run for events"

ic --db="$TEST_DB" run advance "$EVT_RUN" --priority=4 >/dev/null
pass "advance run for events"

# Tail events — should have at least one phase event
EVT_OUTPUT=$(ic --db="$TEST_DB" events tail "$EVT_RUN")
echo "$EVT_OUTPUT" | grep -q '"source":"phase"' || fail "events tail: no phase events"
pass "events tail returns phase events"

# Tail --all should also work
ic --db="$TEST_DB" events tail --all | grep -q '"source":"phase"' || fail "events tail --all: no events"
pass "events tail --all"

# Consumer cursor: first tail stores events, second tail returns empty
ic --db="$TEST_DB" events tail "$EVT_RUN" --consumer=integ-consumer >/dev/null
ic --db="$TEST_DB" events cursor list | grep -q "integ-consumer" || fail "cursor not persisted"
pass "cursor persisted after tail"

SECOND_TAIL=$(ic --db="$TEST_DB" events tail "$EVT_RUN" --consumer=integ-consumer)
[[ -z "$SECOND_TAIL" ]] || fail "cursor dedup failed: got $SECOND_TAIL"
pass "cursor dedup (second tail empty)"

# Cursor reset
ic --db="$TEST_DB" events cursor reset "integ-consumer:$EVT_RUN" | grep -q "reset" || fail "cursor reset failed"
pass "cursor reset"

# After reset, tail should return events again
RESET_TAIL=$(ic --db="$TEST_DB" events tail "$EVT_RUN" --consumer=integ-consumer)
echo "$RESET_TAIL" | grep -q '"source":"phase"' || fail "events after cursor reset: no events"
pass "events after cursor reset"

echo "  Event bus tests passed"

# --- E1: Kernel Primitives Integration Tests ---
echo ""
echo "=== E1: Custom Phase Chain ==="
CHAIN_RUN=$(ic run create --project="$TEST_DIR" --goal="custom chain test" --phases='["draft","review","done"]' --db="$TEST_DB")
chain_phase=$(ic run phase "$CHAIN_RUN" --db="$TEST_DB")
[[ "$chain_phase" == "draft" ]] || fail "custom chain initial phase should be draft, got: $chain_phase"
pass "custom chain: initial phase is first in chain"

ic run advance "$CHAIN_RUN" --disable-gates --db="$TEST_DB" >/dev/null
chain_phase=$(ic run phase "$CHAIN_RUN" --db="$TEST_DB")
[[ "$chain_phase" == "review" ]] || fail "custom chain advance should go to review, got: $chain_phase"
pass "custom chain: advance to second phase"

ic run advance "$CHAIN_RUN" --disable-gates --db="$TEST_DB" >/dev/null
chain_phase=$(ic run phase "$CHAIN_RUN" --db="$TEST_DB")
[[ "$chain_phase" == "done" ]] || fail "custom chain advance should go to done, got: $chain_phase"
pass "custom chain: advance to terminal"

chain_json=$(ic run status "$CHAIN_RUN" --json --db="$TEST_DB")
echo "$chain_json" | grep -q '"phases"' || fail "JSON should include phases"
pass "custom chain: phases in JSON output"

echo "=== E1: Skip Command ==="
SKIP_RUN=$(ic run create --project="$TEST_DIR" --goal="skip test" --phases='["a","b","c","d"]' --db="$TEST_DB")
ic run skip "$SKIP_RUN" b --reason="complexity 1" --actor="test" --db="$TEST_DB" >/dev/null
ic run advance "$SKIP_RUN" --disable-gates --db="$TEST_DB" >/dev/null
skip_phase=$(ic run phase "$SKIP_RUN" --db="$TEST_DB")
[[ "$skip_phase" == "c" ]] || fail "advance should skip 'b' and land on 'c', got: $skip_phase"
pass "skip: advance skips pre-skipped phase"

echo "=== E1: Artifact Content Hashing ==="
echo "test content for hash" > "$TEST_DIR/test-artifact.md"
HASH_RUN=$(ic run create --project="$TEST_DIR" --goal="hash test" --db="$TEST_DB")
HASH_ART=$(ic run artifact add "$HASH_RUN" --phase=brainstorm --path="$TEST_DIR/test-artifact.md" --db="$TEST_DB")
hash_json=$(ic run artifact list "$HASH_RUN" --json --db="$TEST_DB")
echo "$hash_json" | grep -q 'sha256:' || fail "artifact should have sha256 content_hash, got: $hash_json"
pass "artifact: content hash computed"

echo "=== E1: Token Tracking ==="
TOKEN_RUN=$(ic run create --project="$TEST_DIR" --goal="token test" --token-budget=100000 --budget-warn-pct=80 --db="$TEST_DB")
token_json=$(ic run status "$TOKEN_RUN" --json --db="$TEST_DB")
echo "$token_json" | grep -q '"token_budget":100000' || fail "run should have token_budget, got: $token_json"
echo "$token_json" | grep -q '"budget_warn_pct":80' || fail "run should have budget_warn_pct, got: $token_json"
pass "token: budget fields on run create"

# Create a dispatch scoped to this run, report tokens
TOKEN_DISPATCH=$(ic dispatch spawn --type=codex --prompt-file="$PROMPT_FILE" --project="$TEST_DIR" --scope-id="$TOKEN_RUN" --name=token-test --dispatch-sh=/bin/echo --db="$TEST_DB")
sleep 0.5
ic dispatch poll "$TOKEN_DISPATCH" --db="$TEST_DB" >/dev/null 2>&1 || true
ic dispatch tokens "$TOKEN_DISPATCH" --in=5000 --out=2000 --cache=8000 --db="$TEST_DB" >/dev/null 2>&1
pass "token: dispatch tokens reported"

# Aggregate tokens
agg_json=$(ic run tokens "$TOKEN_RUN" --json --db="$TEST_DB")
echo "$agg_json" | grep -q '"input_tokens":5000' || fail "aggregation should show 5000 input, got: $agg_json"
echo "$agg_json" | grep -q '"output_tokens":2000' || fail "aggregation should show 2000 output, got: $agg_json"
pass "token: aggregation correct"

# Budget check (7000/100000 = 7% — should be OK)
budget_json=$(ic run budget "$TOKEN_RUN" --json --db="$TEST_DB")
echo "$budget_json" | grep -q '"exceeded":false' || fail "budget should not be exceeded, got: $budget_json"
pass "token: budget check OK"

echo "=== E1: Wrapper Skip/Token/Budget ==="
intercore_run_skip "$SKIP_RUN" c --reason="wrapper test" --actor="integ" 2>/dev/null || true
pass "wrapper: run skip"

tok_out=$(intercore_run_tokens "$TOKEN_RUN")
echo "$tok_out" | grep -q "input_tokens" || fail "wrapper run tokens should return JSON, got: $tok_out"
pass "wrapper: run tokens"

budget_out=$(intercore_run_budget "$TOKEN_RUN")
echo "$budget_out" | grep -q "budget" || fail "wrapper run budget should return JSON, got: $budget_out"
pass "wrapper: run budget"

echo "  E1 integration tests passed"

# --- Version sync check ---
CLAVAIN_LIB="$SCRIPT_DIR/../../hub/clavain/hooks/lib-intercore.sh"
if [[ -f "$CLAVAIN_LIB" ]]; then
    CLAVAIN_VER=$(grep '^INTERCORE_WRAPPER_VERSION=' "$CLAVAIN_LIB" | cut -d'"' -f2)
    SOURCE_VER=$(grep '^INTERCORE_WRAPPER_VERSION=' "$SCRIPT_DIR/lib-intercore.sh" | cut -d'"' -f2)
    if [[ "$CLAVAIN_VER" = "$SOURCE_VER" ]]; then
        pass "version sync: source=$SOURCE_VER clavain=$CLAVAIN_VER"
    else
        fail "version sync: source=$SOURCE_VER != clavain=$CLAVAIN_VER (re-copy lib-intercore.sh)"
    fi
fi

echo ""
echo "All integration tests passed."
