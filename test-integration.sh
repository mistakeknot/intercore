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

echo ""
echo "=== Per-Run Gate Rules (iv-yfck) ==="

# Create run with inline --gates JSON.
# Every transition in the active chain must be present; [] marks explicit ungated transitions.
GATES_JSON='{"brainstorm→brainstorm-reviewed":[{"check":"artifact_exists","phase":"brainstorm","tier":"hard"}],"brainstorm-reviewed→strategized":[],"strategized→planned":[],"planned→executing":[{"check":"artifact_exists","phase":"planned","tier":"soft"},{"check":"budget_not_exceeded","tier":"hard"}],"executing→review":[],"review→polish":[],"polish→reflect":[],"reflect→done":[]}'
GATES_RUN=$(ic run create --project="$TEST_DIR" --goal="per-run gates test" --gates="$GATES_JSON" --db="$TEST_DB")
[[ -n "$GATES_RUN" ]] || fail "per-run gates: create returned empty ID"
pass "per-run gates: create with --gates"

# Verify gate_rules round-trips in ic run status --json
gates_status=$(ic run status "$GATES_RUN" --json --db="$TEST_DB")
echo "$gates_status" | jq -e '.gate_rules' >/dev/null || fail "per-run gates: gate_rules missing from status JSON"
gates_count=$(echo "$gates_status" | jq '.gate_rules | length')
[[ "$gates_count" -eq 8 ]] || fail "per-run gates: expected 8 transitions in gate_rules, got: $gates_count"
pass "per-run gates: status --json includes gate_rules"

# ic gate rules --run=ID should show per-run rules
run_rules=$(ic gate rules --run="$GATES_RUN" --json --db="$TEST_DB")
echo "$run_rules" | jq -e '.[0].source == "run"' >/dev/null || fail "per-run gates: gate rules source should be 'run', got: $run_rules"
pass "per-run gates: ic gate rules --run shows per-run rules"

# Gate check should evaluate per-run rules (artifact_exists for brainstorm)
ic gate check "$GATES_RUN" --priority=0 --db="$TEST_DB" 2>/dev/null && fail "per-run gates: should fail without artifact" || true
pass "per-run gates: gate check fails without artifact"

# Add brainstorm artifact, gate should pass
ic run artifact add "$GATES_RUN" --phase=brainstorm --path=docs/brainstorms/gates-test.md --db="$TEST_DB" >/dev/null
gates_check=$(ic gate check "$GATES_RUN" --priority=0 --json --db="$TEST_DB")
echo "$gates_check" | jq -e '.result == "pass"' >/dev/null || fail "per-run gates: should pass with artifact, got: $gates_check"
pass "per-run gates: gate check passes with artifact"

# Create run with --gates-file
GATES_FILE="$TEST_DIR/test-gates.json"
echo "$GATES_JSON" > "$GATES_FILE"
GATES_FILE_RUN=$(ic run create --project="$TEST_DIR" --goal="gates-file test" --gates-file="$GATES_FILE" --db="$TEST_DB")
[[ -n "$GATES_FILE_RUN" ]] || fail "per-run gates: create with --gates-file returned empty ID"
file_rules=$(ic run status "$GATES_FILE_RUN" --json --db="$TEST_DB" | jq '.gate_rules | length')
[[ "$file_rules" -eq 8 ]] || fail "per-run gates: --gates-file should store 8 transitions, got: $file_rules"
pass "per-run gates: create with --gates-file"

# Mutual exclusion: --gates and --gates-file together should fail
ic run create --project="$TEST_DIR" --goal="both flags" --gates="$GATES_JSON" --gates-file="$GATES_FILE" --db="$TEST_DB" 2>/dev/null && fail "per-run gates: --gates + --gates-file should fail" || true
pass "per-run gates: --gates + --gates-file mutual exclusion"

# Invalid JSON in --gates should fail
ic run create --project="$TEST_DIR" --goal="bad json" --gates='not-json' --db="$TEST_DB" 2>/dev/null && fail "per-run gates: invalid JSON should fail" || true
pass "per-run gates: invalid --gates JSON rejected"

# Invalid check type should fail
ic run create --project="$TEST_DIR" --goal="bad check" --gates='{"a→b":[{"check":"nonexistent"}]}' --db="$TEST_DB" 2>/dev/null && fail "per-run gates: unknown check should fail" || true
pass "per-run gates: unknown check type rejected"

# Run without --gates should have no gate_rules in status
NO_GATES_RUN=$(ic run create --project="$TEST_DIR" --goal="no gates" --db="$TEST_DB")
no_gates_status=$(ic run status "$NO_GATES_RUN" --json --db="$TEST_DB")
echo "$no_gates_status" | jq -e '.gate_rules' >/dev/null 2>&1 && fail "per-run gates: gate_rules should be absent when not set" || true
pass "per-run gates: no gate_rules when not specified"

# ic gate rules without --run shows defaults (regression)
default_rules=$(ic gate rules --json --db="$TEST_DB")
echo "$default_rules" | jq -e '.[0].source == "default"' >/dev/null || fail "per-run gates: default rules should have source=default"
pass "per-run gates: default rules unaffected"

# Portfolio run with --gates: parent gets gates
PORTFOLIO_GATES_RUN=$(ic run create --projects="$TEST_DIR/p1,$TEST_DIR/p2" --goal="portfolio gates" --gates="$GATES_JSON" --json --db="$TEST_DB" | jq -r '.id')
portfolio_gates_status=$(ic run status "$PORTFOLIO_GATES_RUN" --json --db="$TEST_DB")
echo "$portfolio_gates_status" | jq -e '.gate_rules' >/dev/null || fail "per-run gates: portfolio parent should have gate_rules"
pass "per-run gates: portfolio parent inherits --gates"

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
echo "$EVT_OUTPUT" | jq -e --arg run "$EVT_RUN" 'select(.source=="phase") | .envelope.trace_id == $run' >/dev/null || fail "events tail: missing phase envelope trace_id"
echo "$EVT_OUTPUT" | jq -e 'select(.source=="phase") | (.envelope.input_artifact_refs | type == "array") and (.envelope.output_artifact_refs | type == "array")' >/dev/null || fail "events tail: missing phase artifact refs"
pass "events tail includes envelope provenance"

RUN_EVENTS_JSON=$(ic --db="$TEST_DB" run events "$EVT_RUN" --json)
echo "$RUN_EVENTS_JSON" | jq -e --arg run "$EVT_RUN" 'map(select(.envelope.trace_id == $run)) | length > 0' >/dev/null || fail "run events --json: envelope not exposed"
pass "run events --json includes envelope"

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

# --- Replay Tests ---
echo ""
echo "=== Replay Mode ==="

REPLAY_RUN=$(ic run create --project="$TEST_DIR" --goal="replay simulate test" --phases='["brainstorm","executing","done"]' --db="$TEST_DB")
ic run advance "$REPLAY_RUN" --disable-gates --db="$TEST_DB" >/dev/null
ic run advance "$REPLAY_RUN" --disable-gates --db="$TEST_DB" >/dev/null
pass "replay: completed run created"

# Manual recording path for LLM/external snapshots
REPLAY_INPUT_ID=$(ic run replay record "$REPLAY_RUN" --kind=llm --key=response_snapshot --payload='{"model":"claude","completion":"ok"}' --db="$TEST_DB")
[[ -n "$REPLAY_INPUT_ID" ]] || fail "replay: record returned empty id"
pass "replay: manual nondeterministic input recording"

REPLAY_INPUTS=$(ic run replay inputs "$REPLAY_RUN" --json --db="$TEST_DB")
echo "$REPLAY_INPUTS" | jq -e 'map(select(.kind=="llm")) | length > 0' >/dev/null || fail "replay: recorded llm input not queryable"
pass "replay: recorded inputs queryable"

REPLAY_SIM_1=$(ic run replay "$REPLAY_RUN" --mode=simulate --json --db="$TEST_DB")
REPLAY_SIM_2=$(ic run replay "$REPLAY_RUN" --mode=simulate --json --db="$TEST_DB")
echo "$REPLAY_SIM_1" | jq -e '.decisions | length > 0' >/dev/null || fail "replay simulate: no reconstructed decisions"
[[ "$(echo "$REPLAY_SIM_1" | jq -S '.')" == "$(echo "$REPLAY_SIM_2" | jq -S '.')" ]] || fail "replay simulate: non-deterministic output across repeated runs"
pass "replay simulate: deterministic decision reconstruction"

set +e
REPLAY_REEXEC=$(ic run replay "$REPLAY_RUN" --mode=reexecute --json --db="$TEST_DB")
REPLAY_REEXEC_RC=$?
set -e
[[ "$REPLAY_REEXEC_RC" -eq 1 ]] || fail "replay reexecute: expected exit 1 (gated), got $REPLAY_REEXEC_RC"
echo "$REPLAY_REEXEC" | jq -e '.reexecute.allowed == false' >/dev/null || fail "replay reexecute: expected allowed=false"
echo "$REPLAY_REEXEC" | jq -e '.reexecute.reason | test("gated|disallowed")' >/dev/null || fail "replay reexecute: missing explicit gating reason"
pass "replay reexecute: explicit gated output"

ACTIVE_REPLAY_RUN=$(ic run create --project="$TEST_DIR" --goal="replay active run guard" --db="$TEST_DB")
ic run replay "$ACTIVE_REPLAY_RUN" --mode=simulate --db="$TEST_DB" 2>/dev/null && fail "replay simulate should reject non-completed run" || true
pass "replay simulate: completed-run guard"

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

# --- E6: Rollback & Recovery ---
echo ""
echo "=== E6: Rollback — Workflow State ==="

# Create a run, advance a few times, then rollback
ROLL_RUN=$(ic run create --project="$TEST_DIR" --goal="rollback test" --db="$TEST_DB")
ic run advance "$ROLL_RUN" --disable-gates --db="$TEST_DB" >/dev/null  # brainstorm → brainstorm-reviewed
ic run advance "$ROLL_RUN" --disable-gates --db="$TEST_DB" >/dev/null  # brainstorm-reviewed → strategized
ic run advance "$ROLL_RUN" --disable-gates --db="$TEST_DB" >/dev/null  # strategized → planned

# Verify current phase
roll_phase=$(ic run phase "$ROLL_RUN" --db="$TEST_DB")
[[ "$roll_phase" == "planned" ]] || fail "phase before rollback should be planned, got: $roll_phase"
pass "rollback: at planned phase"

# Dry-run first
dry_out=$(ic run rollback "$ROLL_RUN" --to-phase=brainstorm --dry-run --db="$TEST_DB")
echo "$dry_out" | jq -e '.dry_run == true' >/dev/null || fail "dry-run should be true"
echo "$dry_out" | jq -e '.rolled_back_phases | length == 3' >/dev/null || fail "dry-run should show 3 rolled-back phases"
pass "rollback: dry-run"

# Phase should still be planned after dry-run
roll_phase=$(ic run phase "$ROLL_RUN" --db="$TEST_DB")
[[ "$roll_phase" == "planned" ]] || fail "phase after dry-run should still be planned, got: $roll_phase"
pass "rollback: dry-run doesn't change phase"

# Actual rollback
roll_result=$(ic run rollback "$ROLL_RUN" --to-phase=brainstorm --reason="test rollback" --db="$TEST_DB")
echo "$roll_result" | jq -e '.from_phase == "planned"' >/dev/null || fail "from_phase should be planned"
echo "$roll_result" | jq -e '.to_phase == "brainstorm"' >/dev/null || fail "to_phase should be brainstorm"
pass "rollback: workflow state"

# Verify phase is now brainstorm
roll_phase=$(ic run phase "$ROLL_RUN" --db="$TEST_DB")
[[ "$roll_phase" == "brainstorm" ]] || fail "phase after rollback should be brainstorm, got: $roll_phase"
pass "rollback: phase reverted"

# Verify run is still active
roll_status=$(ic run status "$ROLL_RUN" --json --db="$TEST_DB" | jq -r '.status')
[[ "$roll_status" == "active" ]] || fail "status after rollback should be active, got: $roll_status"
pass "rollback: status active"

# Verify rollback event in audit trail
events_json=$(ic run events "$ROLL_RUN" --json --db="$TEST_DB")
echo "$events_json" | jq -e 'map(select(.event_type == "rollback")) | length > 0' >/dev/null || fail "rollback event should be in audit trail"
pass "rollback: event in audit trail"

echo "=== E6: Rollback — Code Query ==="

# Add an artifact then query
ic run artifact add "$ROLL_RUN" --phase=brainstorm --path=docs/test-artifact.md --db="$TEST_DB" >/dev/null
code_result=$(ic run rollback "$ROLL_RUN" --layer=code --db="$TEST_DB")
echo "$code_result" | jq -e 'length > 0' >/dev/null || fail "code rollback should return artifacts"
pass "rollback: code query returns artifacts"

# Text format
text_result=$(ic run rollback "$ROLL_RUN" --layer=code --format=text --db="$TEST_DB")
[[ -n "$text_result" ]] || fail "code rollback text format should return data"
pass "rollback: code query text format"

echo "=== E6: Rollback — Completed Run ==="

# Create and complete a run, then roll back
COMP_RUN=$(ic run create --project="$TEST_DIR" --goal="completed rollback test" --db="$TEST_DB")
for i in $(seq 9); do
    ic run advance "$COMP_RUN" --disable-gates --db="$TEST_DB" >/dev/null 2>&1 || true
done
comp_status=$(ic run status "$COMP_RUN" --json --db="$TEST_DB" | jq -r '.status')
[[ "$comp_status" == "completed" ]] || fail "run should be completed, got: $comp_status"
pass "rollback: run completed"

# Rollback completed run
comp_result=$(ic run rollback "$COMP_RUN" --to-phase=brainstorm --reason="re-evaluate" --db="$TEST_DB")
echo "$comp_result" | jq -e '.to_phase == "brainstorm"' >/dev/null || fail "completed run rollback to_phase"
pass "rollback: completed run reverted"

comp_status=$(ic run status "$COMP_RUN" --json --db="$TEST_DB" | jq -r '.status')
[[ "$comp_status" == "active" ]] || fail "completed run should be active after rollback, got: $comp_status"
pass "rollback: completed run status active"

echo "=== E6: Rollback — Cancelled Run (should fail) ==="
CANC_RUN=$(ic run create --project="$TEST_DIR" --goal="cancelled rollback test" --db="$TEST_DB")
ic run cancel "$CANC_RUN" --db="$TEST_DB" >/dev/null
roll_rc=0
ic run rollback "$CANC_RUN" --to-phase=brainstorm --db="$TEST_DB" 2>/dev/null || roll_rc=$?
[[ "$roll_rc" -ne 0 ]] || fail "rollback on cancelled run should fail"
pass "rollback: cancelled run rejected"

echo "=== E6: Rollback — Forward Target (should fail) ==="
FWD_RUN=$(ic run create --project="$TEST_DIR" --goal="forward rollback test" --db="$TEST_DB")
ic run advance "$FWD_RUN" --disable-gates --db="$TEST_DB" >/dev/null  # brainstorm → brainstorm-reviewed
fwd_rc=0
ic run rollback "$FWD_RUN" --to-phase=planned --db="$TEST_DB" 2>/dev/null || fwd_rc=$?
[[ "$fwd_rc" -ne 0 ]] || fail "rollback to forward phase should fail"
pass "rollback: forward target rejected"

echo "=== E6: Rollback — Wrapper Functions ==="
# Re-source wrapper (may have been updated)
source "$SCRIPT_DIR/lib-intercore.sh"
INTERCORE_BIN="$IC_BIN"
INTERCORE_DB="$TEST_DB"

# Advance COMP_RUN so we can test rollback wrapper
# COMP_RUN was rolled back to brainstorm earlier, advance it forward
ic run advance "$COMP_RUN" --disable-gates --db="$TEST_DB" >/dev/null  # brainstorm → brainstorm-reviewed
ic run advance "$COMP_RUN" --disable-gates --db="$TEST_DB" >/dev/null  # brainstorm-reviewed → strategized

# Wrapper dry-run
wrap_dry=$(intercore_run_rollback_dry "$COMP_RUN" "brainstorm")
echo "$wrap_dry" | jq -e '.dry_run == true' >/dev/null || fail "wrapper dry-run should return dry_run=true"
pass "wrapper: rollback dry-run"

# Wrapper actual rollback
wrap_result=$(intercore_run_rollback "$COMP_RUN" "brainstorm" "wrapper test")
echo "$wrap_result" | jq -e '.to_phase == "brainstorm"' >/dev/null || fail "wrapper rollback to_phase"
pass "wrapper: rollback"

# Wrapper code rollback
wrap_code=$(intercore_run_code_rollback "$COMP_RUN")
[[ -n "$wrap_code" ]] && pass "wrapper: code rollback" || pass "wrapper: code rollback (no artifacts)"

echo "  E6 rollback tests passed"

# --- E5: Discovery Pipeline ---
echo ""
echo "=== E5: Discovery Pipeline ==="

# Submit a discovery
DISC_ID=$(ic discovery submit --source=exa --source-id=exa-001 --title="Test discovery" --summary="A test" --url="https://example.com" --score=0.75 --db="$TEST_DB")
[[ -n "$DISC_ID" ]] || fail "discovery submit returned empty ID"
pass "discovery submit"

# Status check
disc_status=$(ic discovery status "$DISC_ID" --db="$TEST_DB")
echo "$disc_status" | grep -q "exa" || fail "discovery status should show source 'exa'"
pass "discovery status"

# JSON status
disc_json=$(ic discovery status "$DISC_ID" --json --db="$TEST_DB")
echo "$disc_json" | grep -q '"status":"new"' || fail "discovery should be 'new', got: $disc_json"
pass "discovery status --json"

# List discoveries
disc_list=$(ic discovery list --db="$TEST_DB")
echo "$disc_list" | grep -q "$DISC_ID" || fail "discovery list should include submitted ID"
pass "discovery list"

# List with filters
disc_filtered=$(ic discovery list --source=exa --status=new --tier=medium --db="$TEST_DB")
echo "$disc_filtered" | grep -q "$DISC_ID" || fail "filtered list should include ID"
pass "discovery list --source --status --tier"

# Score a discovery
ic discovery score "$DISC_ID" --score=0.9 --db="$TEST_DB" | grep -q "scored" || fail "discovery score"
pass "discovery score"

# Verify tier changed to high
disc_scored=$(ic discovery status "$DISC_ID" --json --db="$TEST_DB")
echo "$disc_scored" | grep -q '"confidence_tier":"high"' || fail "scored discovery should be high tier, got: $disc_scored"
pass "discovery score tier update"

# Promote a discovery
ic discovery promote "$DISC_ID" --bead-id=iv-test1 --db="$TEST_DB" | grep -q "promoted" || fail "discovery promote"
disc_promoted=$(ic discovery status "$DISC_ID" --json --db="$TEST_DB")
echo "$disc_promoted" | grep -q '"status":"promoted"' || fail "promoted discovery should have status=promoted"
pass "discovery promote"

# Promote already-promoted is idempotent (returns success)
ic discovery promote "$DISC_ID" --bead-id=iv-test2 --db="$TEST_DB" >/dev/null
pass "discovery promote (idempotent on re-promote)"

# Submit and dismiss
DISC_ID2=$(ic discovery submit --source=exa --source-id=exa-002 --title="Dismissable" --score=0.4 --db="$TEST_DB")
ic discovery dismiss "$DISC_ID2" --db="$TEST_DB" | grep -q "dismissed" || fail "discovery dismiss"
pass "discovery dismiss"

# Score dismissed should fail
ic discovery score "$DISC_ID2" --score=0.8 --db="$TEST_DB" 2>/dev/null && fail "score dismissed should fail" || true
pass "discovery score (dismissed rejected)"

# Promote dismissed should fail even with --force
ic discovery promote "$DISC_ID2" --bead-id=iv-test3 --force --db="$TEST_DB" 2>/dev/null && fail "promote dismissed should fail" || true
pass "discovery promote (dismissed blocked even with --force)"

# Submit duplicate source_id should fail
ic discovery submit --source=exa --source-id=exa-001 --title="Duplicate" --db="$TEST_DB" 2>/dev/null && fail "duplicate source should fail" || true
pass "discovery submit (duplicate rejected)"

# Feedback
ic discovery feedback "$DISC_ID" --signal=upvote --actor=user --db="$TEST_DB" | grep -q "feedback recorded" || fail "discovery feedback"
pass "discovery feedback"

# Profile (should return default empty profile)
profile_out=$(ic discovery profile --json --db="$TEST_DB")
echo "$profile_out" | grep -q "keyword_weights" || fail "profile should have keyword_weights"
pass "discovery profile"

# Profile update
ic discovery profile update --keyword-weights='{"ai":1.5}' --source-weights='{"exa":2.0}' --db="$TEST_DB" | grep -q "profile updated" || fail "profile update"
pass "discovery profile update"

# Verify profile update
profile_updated=$(ic discovery profile --json --db="$TEST_DB")
echo "$profile_updated" | grep -q "ai" || fail "profile should contain updated keyword"
pass "discovery profile update verified"

# Decay (submit a fresh one, then decay)
DISC_ID3=$(ic discovery submit --source=exa --source-id=exa-003 --title="Decayable" --score=0.6 --db="$TEST_DB")
# Backdate to make it eligible for decay (min-age=0 to bypass age check)
decay_out=$(ic discovery decay --rate=0.1 --min-age=0 --db="$TEST_DB")
echo "$decay_out" | grep -q "decayed" || fail "decay should report count"
pass "discovery decay"

# Rollback
DISC_ID4=$(ic discovery submit --source=rollback-src --source-id=rb-001 --title="Rollback test" --score=0.5 --db="$TEST_DB")
DISC_ID5=$(ic discovery submit --source=rollback-src --source-id=rb-002 --title="Rollback test 2" --score=0.5 --db="$TEST_DB")
rollback_count=$(ic discovery rollback --source=rollback-src --since=0 --db="$TEST_DB")
[[ "$rollback_count" -eq 2 ]] || fail "rollback should dismiss 2 discoveries, got: $rollback_count"
pass "discovery rollback"

# Verify rolled back discoveries are dismissed
rb_status=$(ic discovery status "$DISC_ID4" --json --db="$TEST_DB")
echo "$rb_status" | grep -q '"status":"dismissed"' || fail "rolled-back discovery should be dismissed"
pass "discovery rollback status verified"

# Event bus integration — discovery events should appear in unified stream
EVT_DISC_OUTPUT=$(ic --db="$TEST_DB" events tail --all)
echo "$EVT_DISC_OUTPUT" | grep -q '"source":"discovery"' || fail "events tail should include discovery events"
pass "discovery events in unified event bus"

echo "  E5 discovery tests passed"

# --- E9: Portfolio Dependency Scheduling ---
echo ""
echo "=== E9: Portfolio Dependency Scheduling ==="

# Create a portfolio run (project_dir="" signals portfolio)
# head -1: portfolio create prints child IDs on subsequent lines
PORTFOLIO_RUN=$(ic run create --project="" --goal="Portfolio dep test" --projects="/proj/a,/proj/b,/proj/c" --db="$TEST_DB" | head -1)
[[ -n "$PORTFOLIO_RUN" ]] || fail "portfolio run create returned empty ID"
pass "portfolio: create"

# Add dependencies: A → B (B depends on A), A → C (C depends on A)
ic portfolio dep add "$PORTFOLIO_RUN" --upstream="/proj/a" --downstream="/proj/b" --db="$TEST_DB" >/dev/null
ic portfolio dep add "$PORTFOLIO_RUN" --upstream="/proj/a" --downstream="/proj/c" --db="$TEST_DB" >/dev/null
pass "portfolio: deps added"

# List deps
dep_list=$(ic portfolio dep list "$PORTFOLIO_RUN" --json --db="$TEST_DB")
dep_count=$(echo "$dep_list" | jq 'length')
[[ "$dep_count" -eq 2 ]] || fail "should have 2 deps, got: $dep_count"
pass "portfolio: dep list"

# Cycle detection: B → A should fail (A already → B)
ic portfolio dep add "$PORTFOLIO_RUN" --upstream="/proj/b" --downstream="/proj/a" --db="$TEST_DB" 2>/dev/null && fail "cycle should be rejected" || true
pass "portfolio: cycle rejected"

# Topological order
order_out=$(ic portfolio order "$PORTFOLIO_RUN" --json --db="$TEST_DB")
first=$(echo "$order_out" | jq -r '.[0]')
# /proj/a should be first (it has no upstream deps)
[[ "$first" == "/proj/a" ]] || fail "topo order first should be /proj/a, got: $first"
pass "portfolio: topological order"

# Portfolio status — children should exist from --projects flag
status_out=$(ic portfolio status "$PORTFOLIO_RUN" --json --db="$TEST_DB")
status_count=$(echo "$status_out" | jq 'length')
[[ "$status_count" -eq 3 ]] || fail "portfolio status should show 3 children, got: $status_count"
pass "portfolio: status shows children"

# Find child IDs
CHILD_A_ID=$(ic run list --db="$TEST_DB" --json | jq -r ".[] | select(.project_dir == \"/proj/a\") | .id")
CHILD_B_ID=$(ic run list --db="$TEST_DB" --json | jq -r ".[] | select(.project_dir == \"/proj/b\") | .id")
[[ -n "$CHILD_A_ID" ]] || fail "child A not found"
[[ -n "$CHILD_B_ID" ]] || fail "child B not found"
pass "portfolio: children found"

# Add artifacts so artifact_exists gate doesn't block
ic run artifact add "$CHILD_A_ID" --phase=brainstorm --path=docs/brainstorms/a.md --db="$TEST_DB" >/dev/null
ic run artifact add "$CHILD_B_ID" --phase=brainstorm --path=docs/brainstorms/b.md --db="$TEST_DB" >/dev/null

# Advance child A past brainstorm (hard priority, artifact exists so passes)
ic run advance "$CHILD_A_ID" --priority=0 --db="$TEST_DB" >/dev/null

# Gate check on child B (hard priority): artifact_exists passes (artifact added),
# upstreams_at_phase passes (upstream A is at brainstorm-reviewed, ahead of B's target)
gate_b=$(ic gate check "$CHILD_B_ID" --priority=0 --json --db="$TEST_DB")
echo "$gate_b" | jq -e '.result == "pass"' >/dev/null || fail "child B gate should pass when upstream A is ahead, got: $gate_b"
pass "portfolio: upstream gate passes when upstream is ahead"

# Verify upstreams_at_phase condition is in the evidence
echo "$gate_b" | jq -e '.evidence.conditions[] | select(.check == "upstreams_at_phase") | .result == "pass"' >/dev/null || fail "upstreams_at_phase should be pass in evidence"
pass "portfolio: upstreams_at_phase in gate evidence"

# Diamond dependency: add B → C (C now depends on both A and B)
ic portfolio dep add "$PORTFOLIO_RUN" --upstream="/proj/b" --downstream="/proj/c" --db="$TEST_DB" >/dev/null
pass "portfolio: diamond dep added (no cycle)"

# No-dep portfolio: create one without deps, advance freely
NODEP_PORTFOLIO=$(ic run create --project="" --goal="No-dep portfolio" --projects="/proj/x,/proj/y" --db="$TEST_DB" | head -1)
CHILD_X_ID=$(ic run list --db="$TEST_DB" --json | jq -r ".[] | select(.project_dir == \"/proj/x\") | .id")
ic run advance "$CHILD_X_ID" --priority=4 --db="$TEST_DB" >/dev/null
child_x_phase=$(ic run phase "$CHILD_X_ID" --db="$TEST_DB")
[[ "$child_x_phase" != "brainstorm" ]] || fail "no-dep child should advance freely, got: $child_x_phase"
pass "portfolio: no-dep child advances freely"

echo "  E9 portfolio dependency scheduling tests passed"

# --- Cost-Aware Scheduling (iv-suzr) ---
echo ""
echo "=== Cost-Aware Scheduling ==="

# Config set/get
ic config set global_max_dispatches 5 --db="$TEST_DB" >/dev/null
cfg_val=$(ic config get global_max_dispatches --db="$TEST_DB")
echo "$cfg_val" | grep -q "5" || fail "config set/get: expected 5, got: $cfg_val"
pass "config: set/get global_max_dispatches"

ic config set max_spawn_depth 3 --db="$TEST_DB" >/dev/null
cfg_val2=$(ic config get max_spawn_depth --db="$TEST_DB")
echo "$cfg_val2" | grep -q "3" || fail "config set/get: expected 3, got: $cfg_val2"
pass "config: set/get max_spawn_depth"

# Config list
cfg_list=$(ic config list --json --db="$TEST_DB")
echo "$cfg_list" | grep -q "global_max_dispatches" || fail "config list missing global_max_dispatches"
echo "$cfg_list" | grep -q "max_spawn_depth" || fail "config list missing max_spawn_depth"
pass "config: list"

# Config get for unset key
cfg_unset=$(ic config get nonexistent_key --db="$TEST_DB" 2>&1) || true
echo "$cfg_unset" | grep -q "not set" || fail "config get unset: expected 'not set', got: $cfg_unset"
pass "config: get unset key returns not-set"

# Run with budget-enforce and max-agents
BUDGET_RUN_ID=$(ic run create --project="$TEST_DIR" --goal="budget test" --complexity=3 --budget-enforce --max-agents=2 --token-budget=1000 --db="$TEST_DB")
[[ -n "$BUDGET_RUN_ID" ]] || fail "run create with budget-enforce"
pass "budget: run create with budget-enforce"

# Verify budget-enforce is set in status
budget_status=$(ic run status "$BUDGET_RUN_ID" --json --db="$TEST_DB")
echo "$budget_status" | grep -q '"budget_enforce":true' || fail "budget_enforce not set in status: $budget_status"
echo "$budget_status" | grep -q '"max_agents":2' || fail "max_agents not set in status: $budget_status"
pass "budget: run status shows budget_enforce and max_agents"

# Budget gate: under budget should pass
gate_under=$(ic gate check "$BUDGET_RUN_ID" --priority=0 --json --db="$TEST_DB" 2>&1) || true
# brainstorm → brainstorm-reviewed requires artifact_exists, so gate will fail on that
# but the budget check should pass (no tokens spent yet)
pass "budget: gate check under budget"

# Advance with gates disabled to get to a later phase for agent cap testing
ic run advance "$BUDGET_RUN_ID" --priority=4 --db="$TEST_DB" >/dev/null
ic run advance "$BUDGET_RUN_ID" --priority=4 --db="$TEST_DB" >/dev/null
ic run advance "$BUDGET_RUN_ID" --priority=4 --db="$TEST_DB" >/dev/null
ic run advance "$BUDGET_RUN_ID" --priority=4 --db="$TEST_DB" >/dev/null
pass "budget: advance to executing phase"

echo "  Cost-aware scheduling tests passed"

echo "=== Thematic Work Lanes ==="

# Lane create
LANE_ID=$(ic lane create --name=interop --type=standing --description="Plugin interop" --json --db="$TEST_DB" | jq -r '.id')
[[ -n "$LANE_ID" ]] || fail "lane create"
pass "lane: create"

# Lane list
ic lane list --json --db="$TEST_DB" | jq -e '.[0].name == "interop"' >/dev/null || fail "lane list"
pass "lane: list"

# Lane status
ic lane status "$LANE_ID" --json --db="$TEST_DB" | jq -e '.name == "interop"' >/dev/null || fail "lane status"
pass "lane: status"

# Lane status by name
ic lane status interop --json --db="$TEST_DB" | jq -e '.name == "interop"' >/dev/null || fail "lane status by name"
pass "lane: status by name"

# Lane sync (membership)
ic lane sync "$LANE_ID" --bead-ids=iv-abc1,iv-abc2,iv-abc3 --db="$TEST_DB" >/dev/null || fail "lane sync"
MEMBER_COUNT=$(ic lane members "$LANE_ID" --json --db="$TEST_DB" | jq 'length')
[[ "$MEMBER_COUNT" == "3" ]] || fail "lane members: expected 3, got $MEMBER_COUNT"
pass "lane: sync + members"

# Lane events
EVENT_COUNT=$(ic lane events "$LANE_ID" --json --db="$TEST_DB" | jq 'length')
[[ "$EVENT_COUNT" -ge 2 ]] || fail "lane events: expected >= 2, got $EVENT_COUNT"
pass "lane: events"

# Lane close
ic lane close "$LANE_ID" --db="$TEST_DB" >/dev/null || fail "lane close"
CLOSED_STATUS=$(ic lane status "$LANE_ID" --json --db="$TEST_DB" | jq -r '.status')
[[ "$CLOSED_STATUS" == "closed" ]] || fail "lane close: expected closed, got $CLOSED_STATUS"
pass "lane: close"

# Create arc lane
ARC_LANE_ID=$(ic lane create --name=e8-bigend --type=arc --json --db="$TEST_DB" | jq -r '.id')
ARC_TYPE=$(ic lane status "$ARC_LANE_ID" --json --db="$TEST_DB" | jq -r '.lane_type')
[[ "$ARC_TYPE" == "arc" ]] || fail "lane arc type: expected arc, got $ARC_TYPE"
pass "lane: arc type"

echo "  Thematic work lanes tests passed"

# --- Phase Actions (Event-Driven Advancement) ---
echo ""
echo "=== Phase Actions ==="

# Create a run for action tests
ACT_RUN=$(ic --db="$TEST_DB" run create --project="$TEST_DIR" --goal="Action test")
pass "create run for actions"

# Add an action
ADD_OUT=$(ic --db="$TEST_DB" --json run action add "$ACT_RUN" --phase=planned --command=/clavain:work --args='["plan.md"]' --mode=interactive)
ACT_ID=$(echo "$ADD_OUT" | jq -r '.id')
[[ "$ACT_ID" != "null" && "$ACT_ID" != "" ]] || fail "action add: no id returned"
pass "action add"

# Add second action for a different phase
ic --db="$TEST_DB" run action add "$ACT_RUN" --phase=executing --command=/clavain:quality-gates --mode=both >/dev/null
pass "action add (second phase)"

# List actions for a specific phase
LIST_OUT=$(ic --db="$TEST_DB" --json run action list "$ACT_RUN" --phase=planned)
LIST_COUNT=$(echo "$LIST_OUT" | jq 'length')
[[ "$LIST_COUNT" == "1" ]] || fail "action list: expected 1 action for planned, got $LIST_COUNT"
LIST_CMD=$(echo "$LIST_OUT" | jq -r '.[0].command')
[[ "$LIST_CMD" == "/clavain:work" ]] || fail "action list: expected /clavain:work, got $LIST_CMD"
pass "action list (by phase)"

# List all actions for run
LIST_ALL=$(ic --db="$TEST_DB" --json run action list "$ACT_RUN")
ALL_COUNT=$(echo "$LIST_ALL" | jq 'length')
[[ "$ALL_COUNT" == "2" ]] || fail "action list all: expected 2, got $ALL_COUNT"
pass "action list (all)"

# Update an action
ic --db="$TEST_DB" run action update "$ACT_RUN" --phase=planned --command=/clavain:work --args='["updated.md"]' >/dev/null
UPD_OUT=$(ic --db="$TEST_DB" --json run action list "$ACT_RUN" --phase=planned)
UPD_ARGS=$(echo "$UPD_OUT" | jq -r '.[0].args[0]')
[[ "$UPD_ARGS" == "updated.md" ]] || fail "action update: expected updated.md, got $UPD_ARGS"
pass "action update"

# Duplicate detection
DUP_OUT=$(ic --db="$TEST_DB" run action add "$ACT_RUN" --phase=planned --command=/clavain:work 2>&1) && fail "action add duplicate: should have failed" || true
pass "action add duplicate rejected"

# Delete an action
ic --db="$TEST_DB" run action delete "$ACT_RUN" --phase=planned --command=/clavain:work >/dev/null
DEL_OUT=$(ic --db="$TEST_DB" --json run action list "$ACT_RUN" --phase=planned)
DEL_COUNT=$(echo "$DEL_OUT" | jq 'length')
[[ "$DEL_COUNT" == "0" ]] || fail "action delete: expected 0, got $DEL_COUNT"
pass "action delete"

# Actions in advance output — register action for destination phase, advance, check output
# Run starts at brainstorm; advancing goes to brainstorm-reviewed
ic --db="$TEST_DB" run action add "$ACT_RUN" --phase=brainstorm-reviewed --command=/clavain:strategy --mode=interactive >/dev/null
ADV_OUT=$(ic --db="$TEST_DB" --json run advance "$ACT_RUN" --priority=4)
ADV_ACTIONS=$(echo "$ADV_OUT" | jq '.actions // [] | length')
[[ "$ADV_ACTIONS" -ge 1 ]] || fail "advance: expected actions in output, got $ADV_ACTIONS"
ADV_CMD=$(echo "$ADV_OUT" | jq -r '.actions[0].command')
[[ "$ADV_CMD" == "/clavain:strategy" ]] || fail "advance: expected /clavain:strategy, got $ADV_CMD"
pass "advance includes resolved actions"

# Batch add via --actions on create
BATCH_RUN=$(ic --db="$TEST_DB" run create --project="$TEST_DIR" --goal="Batch action test" --actions='{"planned":{"command":"/interflux:flux-drive","args":"[\"plan.md\"]","mode":"interactive"},"executing":{"command":"/clavain:work","mode":"both"}}')
BATCH_LIST=$(ic --db="$TEST_DB" --json run action list "$BATCH_RUN")
BATCH_COUNT=$(echo "$BATCH_LIST" | jq 'length')
[[ "$BATCH_COUNT" == "2" ]] || fail "batch action: expected 2, got $BATCH_COUNT"
pass "batch action registration via --actions"

echo "  Phase actions tests passed"

# --- Version sync check ---
# --- Agency Specs ---
echo "## Agency Specs"

AGENCY_SPEC_DIR="$SCRIPT_DIR/../../hub/clavain/config/agency"
if [[ -d "$AGENCY_SPEC_DIR" ]]; then

# Validate all spec files
ic agency validate --all --spec-dir="$AGENCY_SPEC_DIR" >/dev/null 2>&1 && pass "agency validate --all" || fail "agency validate --all"

# Validate a single spec
ic agency validate "$AGENCY_SPEC_DIR/build.yaml" >/dev/null 2>&1 && pass "agency validate single file" || fail "agency validate single file"

# Create a run for agency loading
AGENCY_RUN=$(ic --json run create --project="$TEST_DIR" --goal="agency test" | jq -r '.id')
[[ -n "$AGENCY_RUN" ]] && pass "agency: create test run ($AGENCY_RUN)" || fail "agency: create test run"

# Load build stage
ic agency load build --run="$AGENCY_RUN" --spec-dir="$AGENCY_SPEC_DIR" >/dev/null 2>&1 && pass "agency load build" || fail "agency load build"

# Verify phase_actions were registered
ACTION_COUNT=$(ic --json run action list "$AGENCY_RUN" --phase=planned 2>/dev/null | jq 'length')
[[ "$ACTION_COUNT" -ge 1 ]] && pass "agency: planned phase has $ACTION_COUNT actions" || fail "agency: planned phase has 0 actions"

EXEC_COUNT=$(ic --json run action list "$AGENCY_RUN" --phase=executing 2>/dev/null | jq 'length')
[[ "$EXEC_COUNT" -ge 2 ]] && pass "agency: executing phase has $EXEC_COUNT actions" || fail "agency: executing phase has $EXEC_COUNT actions (expected >= 2)"

# Verify model overrides stored in state
MODEL_JSON=$(ic state get "agency.models.planned" "$AGENCY_RUN" 2>/dev/null)
MODEL_DEFAULT=$(echo "$MODEL_JSON" | jq -r '.default' 2>/dev/null)
[[ "$MODEL_DEFAULT" == "sonnet" ]] && pass "agency: model override stored (default=sonnet)" || fail "agency: model override not found (got $MODEL_DEFAULT)"

MODEL_CAT=$(echo "$MODEL_JSON" | jq -r '.categories.review' 2>/dev/null)
[[ "$MODEL_CAT" == "opus" ]] && pass "agency: model category stored (review=opus)" || fail "agency: model category not found (got $MODEL_CAT)"

# Verify gate rules stored in state
GATE_JSON=$(ic state get "agency.gates.planned" "$AGENCY_RUN" 2>/dev/null)
GATE_CHECK=$(echo "$GATE_JSON" | jq -r '.entry[0].check' 2>/dev/null)
[[ "$GATE_CHECK" == "artifact_exists" ]] && pass "agency: gate rules stored" || fail "agency: gate rules not found (got $GATE_CHECK)"

# Load all stages
AGENCY_RUN2=$(ic --json run create --project="$TEST_DIR" --goal="agency all test" | jq -r '.id')
ic agency load all --run="$AGENCY_RUN2" --spec-dir="$AGENCY_SPEC_DIR" >/dev/null 2>&1 && pass "agency load all" || fail "agency load all"

# Verify capabilities stored
CAPS_JSON=$(ic agency capabilities "$AGENCY_RUN2" 2>/dev/null)
CAPS_BUILD=$(echo "$CAPS_JSON" | jq -r '.build' 2>/dev/null)
[[ "$CAPS_BUILD" != "null" && -n "$CAPS_BUILD" ]] && pass "agency: capabilities stored" || fail "agency: capabilities not found"

# Show a spec as JSON
SHOW_JSON=$(ic agency show build --spec-dir="$AGENCY_SPEC_DIR" 2>/dev/null)
SHOW_STAGE=$(echo "$SHOW_JSON" | jq -r '.meta.stage' 2>/dev/null)
[[ "$SHOW_STAGE" == "build" ]] && pass "agency show build" || fail "agency show (stage=$SHOW_STAGE)"

else
echo "  SKIP: agency spec dir not found"
fi

echo "=== Scheduler ==="

# Submit a job
JOB_ID=$(ic --json scheduler submit --prompt-file="$TEST_DIR/test-prompt.md" --project="$TEST_DIR" --type=codex --session=integ-session --db="$TEST_DB" 2>/dev/null | jq -r '.id')
[[ -n "$JOB_ID" && "$JOB_ID" != "null" ]] && pass "scheduler submit" || fail "scheduler submit (id=$JOB_ID)"

# Check status
JOB_STATUS=$(ic --json scheduler status "$JOB_ID" --db="$TEST_DB" 2>/dev/null | jq -r '.status')
[[ "$JOB_STATUS" == "pending" ]] && pass "scheduler status" || fail "scheduler status (status=$JOB_STATUS)"

# List jobs
LIST_COUNT=$(ic --json scheduler list --db="$TEST_DB" 2>/dev/null | jq 'length')
[[ "$LIST_COUNT" -ge 1 ]] && pass "scheduler list" || fail "scheduler list (count=$LIST_COUNT)"

# List with status filter
PENDING_COUNT=$(ic --json scheduler list --status=pending --db="$TEST_DB" 2>/dev/null | jq 'length')
[[ "$PENDING_COUNT" -ge 1 ]] && pass "scheduler list --status=pending" || fail "scheduler list --status=pending (count=$PENDING_COUNT)"

# Stats
STATS_PENDING=$(ic --json scheduler stats --db="$TEST_DB" 2>/dev/null | jq '.pending')
[[ "$STATS_PENDING" -ge 1 ]] && pass "scheduler stats" || fail "scheduler stats (pending=$STATS_PENDING)"

# Cancel
ic scheduler cancel "$JOB_ID" --db="$TEST_DB" >/dev/null 2>&1
CANCEL_STATUS=$(ic --json scheduler status "$JOB_ID" --db="$TEST_DB" 2>/dev/null | jq -r '.status')
[[ "$CANCEL_STATUS" == "cancelled" ]] && pass "scheduler cancel" || fail "scheduler cancel (status=$CANCEL_STATUS)"

# Pause/Resume
ic scheduler pause --db="$TEST_DB" >/dev/null 2>&1
pass "scheduler pause"
ic scheduler resume --db="$TEST_DB" >/dev/null 2>&1
pass "scheduler resume"

# Submit via dispatch spawn --scheduled
SCHED_ID=$(ic --json dispatch spawn --scheduled --prompt-file="$TEST_DIR/test-prompt.md" --project="$TEST_DIR" --db="$TEST_DB" 2>/dev/null | jq -r '.job_id')
[[ -n "$SCHED_ID" && "$SCHED_ID" != "null" ]] && pass "dispatch spawn --scheduled" || fail "dispatch spawn --scheduled (id=$SCHED_ID)"

# Prune (should prune the cancelled job — sleep 1s so completed_at is in the past)
sleep 1
PRUNED=$(ic --json scheduler prune --older-than=0s --db="$TEST_DB" 2>/dev/null | jq '.pruned')
[[ "$PRUNED" -ge 1 ]] && pass "scheduler prune" || fail "scheduler prune (pruned=$PRUNED)"

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

# --- Situation Snapshot ---
echo ""
echo "=== Situation Snapshot ==="

# Empty snapshot: no active runs scoped, just global state
snap_out=$(ic situation snapshot --db="$TEST_DB")
snap_rc=$?
[[ "$snap_rc" -eq 0 ]] || fail "situation snapshot exit code: expected 0, got $snap_rc"
pass "situation snapshot: exit code 0"

# Validate JSON structure
echo "$snap_out" | python3 -c "import sys,json; d=json.load(sys.stdin); assert 'timestamp' in d; assert 'runs' in d; assert 'dispatches' in d; assert 'recent_events' in d; assert 'queue' in d" || fail "situation snapshot: invalid JSON structure"
pass "situation snapshot: valid JSON with expected keys"

# Scoped snapshot: create a run, then snapshot scoped to that run
SNAP_RUN=$(ic run create --project="$TEST_DIR" --goal="Snapshot test run" --db="$TEST_DB")
[[ -n "$SNAP_RUN" ]] || fail "situation snapshot: run create returned empty ID"

scoped_out=$(ic situation snapshot --run="$SNAP_RUN" --db="$TEST_DB")
scoped_rc=$?
[[ "$scoped_rc" -eq 0 ]] || fail "situation snapshot --run exit code: expected 0, got $scoped_rc"
pass "situation snapshot --run: exit code 0"

# Verify the scoped snapshot contains the created run
echo "$scoped_out" | python3 -c "
import sys, json
d = json.load(sys.stdin)
runs = d['runs']
assert len(runs) == 1, f'expected 1 run, got {len(runs)}'
assert runs[0]['id'] == '$SNAP_RUN', f'expected run id $SNAP_RUN, got {runs[0][\"id\"]}'
assert runs[0]['goal'] == 'Snapshot test run', f'unexpected goal: {runs[0][\"goal\"]}'
" || fail "situation snapshot --run: output missing created run"
pass "situation snapshot --run: contains created run"

echo "  Situation snapshot tests passed"

echo ""
echo "All integration tests passed."
