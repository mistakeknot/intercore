#!/usr/bin/env bash
set -euo pipefail
# core/intercore/interlab.sh — wraps Intercore Go benchmarks for interlab consumption.
# Metric: topo_sort_ns (BenchmarkTopologicalSort50)

MONOREPO="$(cd "$(dirname "$0")/../.." && pwd)"
HARNESS="${INTERLAB_HARNESS:-$MONOREPO/interverse/interlab/scripts/go-bench-harness.sh}"
DIR="$(cd "$(dirname "$0")" && pwd)"

echo "--- topo sort ---" >&2
bash "$HARNESS" --pkg ./internal/portfolio/ --bench 'BenchmarkTopologicalSort50$' --metric topo_sort_ns --dir "$DIR"
