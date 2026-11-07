package routing

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const tablesGolden = "testdata/tables_characterization.golden"

func renderEffortTable() string {
	var b strings.Builder
	fmt.Fprintf(&b, "effort levels: %s\n", strings.Join(effortLevels, ","))
	keys := make([]string, 0, len(backendEffortOrders))
	for k := range backendEffortOrders {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Fprintf(&b, "backend effort orders (%d):\n", len(keys))
	for _, k := range keys {
		fmt.Fprintf(&b, "  %s => %s\n", k, strings.Join(backendEffortOrders[k], ","))
	}
	probes := []struct{ backend, identity string }{
		{"codex", "gpt-6-astra"}, {"codex", "gpt-6-sol"}, {"codex", "gpt-6.1-sol"}, {"codex", "gpt-6.1-astra"},
		{"codex", "gpt-6"}, {"codex", "gpt-6."}, {"codex", "gpt-5.6-sol"}, {"codex", "claude-opus-5-5"},
		{"claude", "claude-opus-5-5"}, {"claude", "claude-sonnet-5-5"}, {"claude", "claude-haiku-5-5"},
		{"claude", "claude-fable-5-1"}, {"claude", "gpt-6-astra"},
		{"kimi", "kimi-code/k3"}, {"kimi", "kimi-code/k30"}, {"kimi", "kimi-code/k3-preview"},
		{"main", "gpt-6-astra"}, {"unknown", "gpt-6-astra"}, {"codex", ""}, {"", ""},
	}
	fmt.Fprintf(&b, "effort order selection:\n")
	for _, p := range probes {
		order, ok := effortOrder(DispatchProfile{Backend: p.backend, ModelIdentity: p.identity})
		fmt.Fprintf(&b, "  backend=%q identity=%q => ok=%t order=%s\n", p.backend, p.identity, ok, strings.Join(order, ","))
	}
	return b.String()
}

func renderLabTable() string {
	var b strings.Builder
	fmt.Fprintf(&b, "model lab by canonical identity:\n")
	for _, id := range []string{
		"gpt-6-astra", "gpt-6.1-sol", "gpt-5.6-sol", "gpt-", "gpt", "GPT-6-astra",
		"claude-opus-5-5", "claude-sonnet-5-5", "claude-haiku-5-5", "claude-fable-5-1", "claude-", "claude",
		"kimi-code/k3", "kimi/k3", "kimi-k3", "kimi", "moonshot-v1", "gemini-3", "o3", "unknown", "",
	} {
		fmt.Fprintf(&b, "  %q => %q\n", id, modelLab(id))
	}
	return b.String()
}

func renderFloorApplication() string {
	var b strings.Builder
	floors := map[string]string{"foundational-invariants": "high", "capability-failure": "xhigh", "broad-consequences": "medium", "weird": "ultra"}
	profiles := []DispatchProfile{
		{Backend: "codex", ModelIdentity: "gpt-6-astra", ReasoningEffort: "low"},
		{Backend: "codex", ModelIdentity: "gpt-6.1-sol", ReasoningEffort: "medium"},
		{Backend: "codex", ModelIdentity: "gpt-6.1-sol", ReasoningEffort: "xhigh"},
		{Backend: "claude", ModelIdentity: "claude-opus-5-5", ReasoningEffort: "medium"},
		{Backend: "claude", ModelIdentity: "claude-sonnet-5-5", ReasoningEffort: "high"},
		{Backend: "claude", ModelIdentity: "claude-sonnet-5-5", ReasoningEffort: "low"},
		{Backend: "kimi", ModelIdentity: "kimi-code/k3", ReasoningEffort: "high"},
		{Backend: "main", ModelIdentity: "gpt-6-astra", ReasoningEffort: "high"},
	}
	reasonSets := [][]string{
		nil,
		{"unresolved-success-criteria"},
		{"broad-consequences"},
		{"foundational-invariants"},
		{"capability-failure"},
		{"foundational-invariants", "capability-failure"},
		{"capability-failure", "foundational-invariants", "foundational-invariants"},
		{"weird"},
	}
	for _, p := range profiles {
		for _, reasons := range reasonSets {
			out, raised, err := applyEffortFloor(p, reasons, floors)
			fmt.Fprintf(&b, "%s/%s %s reasons=%v => effort=%s raised=%v err=%v\n",
				p.Backend, p.ModelIdentity, p.ReasoningEffort, reasons, out.ReasoningEffort, raised, err)
		}
	}
	return b.String()
}

func TestTablesCharacterization(t *testing.T) {
	got := renderEffortTable() + "\n" + renderLabTable() + "\n" + renderFloorApplication()
	if os.Getenv("ROUTING_CHARACTERIZATION_UPDATE") == "1" {
		if err := os.MkdirAll(filepath.Dir(tablesGolden), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(tablesGolden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(tablesGolden)
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != got {
		t.Fatalf("effort and lab tables changed.\n--- want\n%s\n--- got\n%s", want, got)
	}
}
