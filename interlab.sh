#!/usr/bin/env bash
set -euo pipefail
# core/intercore/interlab.sh — wraps Intercore Go benchmarks for interlab consumption.
# Primary metric: scoring_assign_ns (BenchmarkAssign20x8)
# Secondary: topo_sort_ns, scoring_pairs_ns

MONOREPO="$(cd "$(dirname "$0")/../.." && pwd)"
HARNESS="${INTERLAB_HARNESS:-$MONOREPO/interverse/interlab/scripts/go-bench-harness.sh}"
DIR="$(cd "$(dirname "$0")" && pwd)"

echo "--- scoring assign ---" >&2
bash "$HARNESS" --pkg ./internal/scoring/ --bench 'BenchmarkAssign20x8$' --metric scoring_assign_ns --dir "$DIR"

echo "--- topo sort ---" >&2
bash "$HARNESS" --pkg ./internal/portfolio/ --bench 'BenchmarkTopologicalSort50$' --metric topo_sort_ns --dir "$DIR"

echo "--- scoring pairs ---" >&2
bash "$HARNESS" --pkg ./internal/scoring/ --bench 'BenchmarkScoreAllPairs10x5$' --metric scoring_pairs_ns --dir "$DIR"
