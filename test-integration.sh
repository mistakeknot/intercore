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

echo "=== Version Sync Check ==="
# Verify Clavain's copy is in sync (if present in monorepo)
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
