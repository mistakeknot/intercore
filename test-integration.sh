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

echo ""
echo "All integration tests passed."
