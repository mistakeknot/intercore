#!/usr/bin/env bash
# hermes-route-adapter.sh — the Hermes-side adapter that routes a task through
# `ic route decide` and applies the decision, honoring the fail-closed contract.
#
# This is the Phase 7 adapter: it makes Hermes (a harness Clavain does not
# control) consume the intercore routing mechanism. Deploy to zklw alongside
# the `ic` binary and the seed registry.
#
# Contract honored (spec §3, fd-arch F2/f-009):
#   - ic route decide exit 0 -> apply the returned model via `hermes model`
#   - ANY non-zero exit  -> HALT. Do NOT fall back to Hermes's default model.
#     (constraint-violation=4 is the DoD "blocks otherwise" clause; a
#      client-confidential task that cannot reach a local model must stop.)
#
# Usage:
#   hermes-route-adapter.sh --class <c> --role <r> [--data <sensitivity>] \
#       [--ic <path-to-ic>] [--registry <path>] -- <hermes task args...>

set -euo pipefail

IC="${IC_BIN:-ic}"
REGISTRY="${IC_REGISTRY:-./registry-seed.yaml}"
CLASS="" ROLE="" DATA=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --class)    CLASS="$2"; shift 2 ;;
    --role)     ROLE="$2"; shift 2 ;;
    --data)     DATA="$2"; shift 2 ;;
    --ic)       IC="$2"; shift 2 ;;
    --registry) REGISTRY="$2"; shift 2 ;;
    --)         shift; break ;;
    *) echo "adapter: unknown flag $1" >&2; exit 2 ;;
  esac
done

if [[ -z "$CLASS" || -z "$ROLE" ]]; then
  echo "adapter: --class and --role are required" >&2
  exit 2
fi

# Build the routing call. ic's flag parser takes --key=value (not --key value),
# so use the equals form. --data is optional (only for sensitive tasks). Note:
# --json is a GLOBAL flag and must precede the subcommand.
route_args=(--json route decide "--class=$CLASS" "--role=$ROLE" "--registry=$REGISTRY")
[[ -n "$DATA" ]] && route_args+=("--data=$DATA")

# Call the mechanism. Capture stdout (decision JSON) and the exit code.
set +e
DECISION="$("$IC" "${route_args[@]}" 2>/tmp/ic-route.err)"
RC=$?
set -e

if [[ $RC -ne 0 ]]; then
  # FAIL CLOSED. This is the load-bearing line: on any routing failure the
  # adapter halts. It never falls through to `hermes model` defaults.
  echo "adapter: ic route decide failed (exit $RC) — HALTING, no fallback" >&2
  cat /tmp/ic-route.err >&2 || true
  case $RC in
    4) echo "adapter: constraint-violation — task blocked by trust-zone policy" >&2 ;;
    1) echo "adapter: no eligible model for this task" >&2 ;;
    3) echo "adapter: malformed routing request" >&2 ;;
  esac
  exit "$RC"
fi

# Decision in hand. Extract the chosen model (vendor/model@deployment).
MODEL="$(printf '%s' "$DECISION" | python3 -c 'import sys,json; print(json.load(sys.stdin)["model"])')"
echo "adapter: routed to $MODEL" >&2

# Apply to Hermes and run the task. `hermes model` switches the active model
# with no code change (per Hermes docs); the remaining args are the task.
hermes model "$MODEL"
exec hermes "$@"
