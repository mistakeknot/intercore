# lib-intercore.sh — Bash wrappers for intercore CLI
# This file is SOURCED by hooks. Do NOT use set -e here — it would exit
# the parent shell on any failure.
# Source in hooks: source "$(dirname "$0")/lib-intercore.sh"
# shellcheck shell=bash

INTERCORE_BIN=""

intercore_available() {
    # Returns 0 (available) or 1 (unavailable).
    # "binary not found" → return 1 (wrappers handle fail-safe individually)
    # "binary found but DB broken" → return 1, log error to stderr
    if [[ -n "$INTERCORE_BIN" ]]; then return 0; fi
    INTERCORE_BIN=$(command -v ic 2>/dev/null || command -v intercore 2>/dev/null)
    if [[ -z "$INTERCORE_BIN" ]]; then
        return 1
    fi
    # Binary exists — check health
    if ! "$INTERCORE_BIN" health >/dev/null 2>&1; then
        printf 'ic: DB health check failed — run '\''ic init'\'' or '\''ic health'\''\n' >&2
        INTERCORE_BIN=""
        return 1
    fi
    return 0
}

intercore_state_set() {
    local key="$1" scope_id="$2" json="$3"
    if ! intercore_available; then return 0; fi
    printf '%s\n' "$json" | "$INTERCORE_BIN" state set "$key" "$scope_id" || return 0
}

intercore_state_get() {
    local key="$1" scope_id="$2"
    if ! intercore_available; then printf ''; return; fi
    "$INTERCORE_BIN" state get "$key" "$scope_id" 2>/dev/null || printf ''
}

intercore_sentinel_check() {
    local name="$1" scope_id="$2" interval="$3"
    if ! intercore_available; then return 0; fi
    "$INTERCORE_BIN" sentinel check "$name" "$scope_id" --interval="$interval" >/dev/null
}
