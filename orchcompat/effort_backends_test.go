package orchcompat

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type effortSeat struct{ role, backend, model, effort string }

var effortSeats = []effortSeat{
	{"seat-astra-low", "codex", "gpt-6-astra", "low"},
	{"seat-astra-medium", "codex", "gpt-6-astra", "medium"},
	{"seat-astra-high", "codex", "gpt-6-astra", "high"},
	{"seat-astra-xhigh", "codex", "gpt-6-astra", "xhigh"},
	{"seat-sol-low", "codex", "gpt-6.1-sol", "low"},
	{"seat-sol-high", "codex", "gpt-6.1-sol", "high"},
	{"seat-opus-medium", "claude", "claude-opus-5-5", "medium"},
	{"seat-opus-high", "claude", "claude-opus-5-5", "high"},
	{"seat-opus-xhigh", "claude", "claude-opus-5-5", "xhigh"},
	{"seat-opus-low", "claude", "claude-opus-5-5", "low"},
	{"seat-sonnet-medium", "claude", "claude-sonnet-5-5", "medium"},
	{"seat-sonnet-high", "claude", "claude-sonnet-5-5", "high"},
	{"seat-kimi-high", "kimi", "kimi-code/k3", "high"},
	{"seat-kimi-medium", "kimi", "kimi-code/k3", "medium"},
	{"seat-astra-on-claude", "claude", "gpt-6-astra", "high"},
	{"seat-opus-on-codex", "codex", "claude-opus-5-5", "high"},
	{"seat-unknown-backend", "other", "gpt-6-astra", "high"},
	{"seat-astra-bogus-effort", "codex", "gpt-6-astra", "turbo"},
}

func effortPolicy(floors string) string {
	var b strings.Builder
	b.WriteString("reasoning:\n  frontier_models: [gpt-6-astra, claude-opus-5-5]\n  frontier_reasons: [unresolved-success-criteria, foundational-invariants, broad-consequences, difficult-verification, capability-failure]\n")
	b.WriteString("dispatch:\n  effort_floors: " + floors + "\n  roles:\n")
	for _, s := range effortSeats {
		fmt.Fprintf(&b, "    %s: t-%s\n", s.role, s.role)
	}
	b.WriteString("  tiers:\n")
	for _, s := range effortSeats {
		fmt.Fprintf(&b, "    t-%s: {role: %s, backend: %s, model: %s, reasoning_effort: %s}\n", s.role, s.role, s.backend, s.model, s.effort)
	}
	return b.String()
}

func TestDispatchEffortFloorsPerBackend(t *testing.T) {
	policies := []struct{ name, floors string }{
		{"floor-high", "{foundational-invariants: high, difficult-verification: high, capability-failure: high}"},
		{"floor-mixed", "{broad-consequences: medium, foundational-invariants: xhigh, capability-failure: low}"},
		{"floor-bogus-level", "{broad-consequences: turbo}"},
	}
	contexts := []string{
		"",
		`{"reasons":["unresolved-success-criteria"],"rationale":"r"}`,
		`{"reasons":["foundational-invariants"],"rationale":"r"}`,
		`{"reasons":["broad-consequences"],"rationale":"r"}`,
		`{"reasons":["difficult-verification"],"rationale":"r"}`,
		`{"reasons":["capability-failure"],"rationale":"r"}`,
		`{"reasons":["broad-consequences","capability-failure"],"rationale":"r"}`,
		`{"reasons":["unresolved-success-criteria","foundational-invariants"],"rationale":"r"}`,
	}
	for _, p := range policies {
		t.Run(p.name, func(t *testing.T) {
			s := newSandbox(t)
			path := filepath.Join(s.root, "policies", p.name+".yaml")
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(effortPolicy(p.floors)), 0o644); err != nil {
				t.Fatal(err)
			}
			var cases []runcase
			for _, seat := range effortSeats {
				for _, ctx := range contexts {
					label := seat.role + " " + seat.backend + "/" + seat.model + "@" + seat.effort
					if ctx == "" {
						label += " reasons=none"
					} else {
						label += " reasons=" + reasonList(ctx)
					}
					cases = append(cases, runcase{Label: label, Args: []string{"--json", "route", "dispatch", "--policy=" + path, "--role=" + seat.role}, Ctx: ctx})
				}
			}
			s.runAll("dispatch-effort-backends-"+p.name, cases)
		})
	}
}

func reasonList(ctx string) string {
	start := strings.Index(ctx, "[") + 1
	end := strings.Index(ctx, "]")
	return strings.ReplaceAll(ctx[start:end], `"`, "")
}
