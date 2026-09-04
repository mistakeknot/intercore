package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// writeRegistry writes a registry YAML to a temp file and returns its path.
func writeRegistry(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "registry.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write registry: %v", err)
	}
	return p
}

const mixedRegistry = `
version: 1
staleness_horizon: 90d
deployments:
  anthropic/claude-fable-5@api:
    version_stability: vendor-live
    trust_zone: vendor-cloud
    cost: { type: per-token, in: 10, out: 50 }
    capabilities:
      discernment: { value: 0.95, provenance: judgment, verified_at: 2026-07-13 }
  nousresearch/hermes-4@zklw:
    version_stability: pinned
    trust_zone: local
    cost: { type: capacity }
    capabilities:
      discernment: { value: 0.60, provenance: judgment, verified_at: 2026-07-13 }
`

const vendorOnlyRegistry = `
version: 1
staleness_horizon: 90d
deployments:
  anthropic/claude-fable-5@api:
    version_stability: vendor-live
    trust_zone: vendor-cloud
    cost: { type: per-token, in: 10, out: 50 }
    capabilities:
      discernment: { value: 0.95, provenance: judgment, verified_at: 2026-07-13 }
`

// TestRouteDecideExitCodes locks the spec's typed exit-code contract (§3,
// Q-3.2) at the CLI boundary a harness calls. The exit-4 case IS the DoD's
// "and blocks otherwise" clause: a client-confidential task with no
// local/approved deployment must fail closed, never fall through.
func TestRouteDecideExitCodes(t *testing.T) {
	mixed := writeRegistry(t, mixedRegistry)
	vendorOnly := writeRegistry(t, vendorOnlyRegistry)
	ctx := context.Background()

	tests := []struct {
		name string
		args []string
		want int
	}{
		{
			name: "client-confidential with a local deployment → decision",
			args: []string{"--class=synthesis", "--role=researcher", "--data=client-confidential", "--registry=" + mixed},
			want: exitDecision,
		},
		{
			name: "non-confidential → decision (all eligible)",
			args: []string{"--class=terminal-grind", "--role=executor", "--registry=" + mixed},
			want: exitDecision,
		},
		{
			name: "client-confidential with NO local deployment → constraint-violation (fail closed)",
			args: []string{"--class=synthesis", "--role=researcher", "--data=client-confidential", "--registry=" + vendorOnly},
			want: exitConstraintViolation,
		},
		{
			name: "missing --role → malformed",
			args: []string{"--class=x", "--registry=" + mixed},
			want: exitMalformed,
		},
		{
			name: "missing registry file → malformed",
			args: []string{"--class=x", "--role=researcher", "--registry=/does/not/exist.yaml"},
			want: exitMalformed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cmdRouteDecide(ctx, tt.args); got != tt.want {
				t.Fatalf("cmdRouteDecide(%v) = %d, want %d", tt.args, got, tt.want)
			}
		})
	}
}
