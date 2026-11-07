package orchcompat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func sortRouteTableJSON(out string) string {
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		return out
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i]["agent"].(string) < rows[j]["agent"].(string) })
	data, _ := json.MarshalIndent(rows, "", "  ")
	return string(data) + "\n"
}

func sortRouteTableText(out string) string {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	header := 0
	for i, l := range lines {
		if strings.HasPrefix(l, "---") {
			header = i + 1
			break
		}
	}
	rest := append([]string(nil), lines[header:]...)
	sort.Strings(rest)
	return strings.Join(append(append([]string(nil), lines[:header]...), rest...), "\n") + "\n"
}

func TestRouteModelMatrix(t *testing.T) {
	s := newSandbox(t)
	phases := []string{"", "brainstorm", "brainstorm-reviewed", "strategized", "strategy-reviewed", "planned", "executing", "shipping", "reflect", "done", "no-such-phase"}
	var cases []runcase
	for _, phase := range phases {
		for _, ac := range [][2]string{{"", ""}, {"research", ""}, {"review", ""}, {"synthesis", ""}, {"explore", ""}, {"workflow", ""}, {"general-purpose", ""}, {"no-such-category", ""}, {"", "fd-safety"}, {"", "fd-architecture"}, {"", "repo-research-analyst"}, {"", "best-practices-researcher"}, {"", "interflux:review:fd-correctness"}, {"review", "fd-correctness"}, {"synthesis", "fd-safety"}} {
			label := "phase=" + phase + " category=" + ac[0] + " agent=" + ac[1]
			cases = append(cases, runcase{
				Label: label,
				Args:  []string{"--json", "route", "model", "--policy={POLICY}", "--phase=" + phase, "--category=" + ac[0], "--agent=" + ac[1]},
			})
		}
	}
	cases = append(cases,
		runcase{Label: "text output", Args: []string{"route", "model", "--policy={POLICY}", "--phase=planned", "--category=review"}},
		runcase{Label: "no flags", Args: []string{"--json", "route", "model", "--policy={POLICY}"}},
	)
	s.runAll("route-model-matrix", cases)
}

func TestRouteModelCalibration(t *testing.T) {
	s := newSandbox(t)
	dir := filepath.Join(s.root, "cal")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"malformed.json": "{not json",
		"empty.json":     "{}",
		"array.json":     "[]",
		"blank.json":     "",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var cases []runcase
	for _, name := range []string{"missing.json", "malformed.json", "empty.json", "array.json", "blank.json"} {
		cal := "--calibration={ROOT}/cal/" + name
		cases = append(cases,
			runcase{Label: "model " + name, Args: []string{"--json", "route", "model", "--policy={POLICY}", "--phase=planned", "--category=review", cal}},
			runcase{Label: "batch " + name, Args: []string{"--json", "route", "batch", "--policy={POLICY}", "--phase=planned", cal, "fd-safety", "fd-architecture"}},
		)
	}
	s.runAll("route-model-calibration", cases)
}

func TestRouteBatch(t *testing.T) {
	s := newSandbox(t)
	cases := []runcase{
		{Label: "json planned", Args: []string{"--json", "route", "batch", "--policy={POLICY}", "--phase=planned", "fd-safety", "fd-architecture", "repo-research-analyst", "other-agent"}},
		{Label: "text planned", Args: []string{"route", "batch", "--policy={POLICY}", "--phase=planned", "fd-safety", "fd-architecture", "repo-research-analyst", "other-agent"}},
		{Label: "json without phase", Args: []string{"--json", "route", "batch", "--policy={POLICY}", "fd-safety", "fd-architecture"}},
		{Label: "json unknown phase", Args: []string{"--json", "route", "batch", "--policy={POLICY}", "--phase=no-such-phase", "fd-safety"}},
		{Label: "duplicate agents", Args: []string{"--json", "route", "batch", "--policy={POLICY}", "--phase=executing", "fd-safety", "fd-safety"}},
		{Label: "no agents", Args: []string{"--json", "route", "batch", "--policy={POLICY}", "--phase=planned"}},
		{Label: "missing policy", Args: []string{"--json", "route", "batch", "--policy={ROOT}/none.yaml", "fd-safety"}},
	}
	s.runAll("route-batch", cases)
}

func TestRouteTable(t *testing.T) {
	s := newSandbox(t)
	var cases []runcase
	for _, phase := range []string{"", "brainstorm", "planned", "executing", "no-such-phase"} {
		label := "phase=" + phase
		jsonArgs := []string{"--json", "route", "table", "--policy={POLICY}"}
		textArgs := []string{"route", "table", "--policy={POLICY}"}
		if phase != "" {
			jsonArgs = append(jsonArgs, "--phase="+phase)
			textArgs = append(textArgs, "--phase="+phase)
		}
		cases = append(cases,
			runcase{Label: "json " + label, Args: jsonArgs, Post: sortRouteTableJSON},
			runcase{Label: "text " + label, Args: textArgs, Post: sortRouteTableText},
		)
	}
	cases = append(cases, runcase{Label: "missing policy", Args: []string{"--json", "route", "table", "--policy={ROOT}/none.yaml"}})
	s.runAll("route-table", cases)
}

func TestRouteIdentity(t *testing.T) {
	s := newSandbox(t)
	models := []string{
		"gpt-6.1-sol", "gpt-6-astra", "GPT-6-ASTRA", "  gpt-6-astra  ", "gpt-6-astra-2026-06-01", "gpt-6-astra-20260601",
		"codex/gpt-6.1-sol", "openai/gpt-6-astra", "openai:gpt-6-astra", "codex/openai/gpt-6-astra",
		"claude-opus-5-5", "claude-opus-5-5[1m]", "claude-opus-5-5[200k]", "claude/claude-opus-5-5", "claude-code/claude-opus-5-5",
		"anthropic/claude-opus-5-5", "claude-opus-5-5-20260601", "claude-sonnet-5-5",
		"opus", "sonnet", "k3", "kimi/k3", "kimi-code/k3", "kimi/kimi-code/k3", "moonshot/kimi-code/k3", "kimi-code/k3-2026-05-01",
		"haiku", "unknown-model", "gpt", "claude", "kimi", "gpt-", "",
	}
	var cases []runcase
	for _, m := range models {
		cases = append(cases, runcase{Label: "model=" + m, Args: []string{"--json", "route", "identity", "--policy={POLICY}", "--model=" + m}})
	}
	cases = append(cases,
		runcase{Label: "text opus", Args: []string{"route", "identity", "--policy={POLICY}", "--model=opus"}},
		runcase{Label: "text unknown", Args: []string{"route", "identity", "--policy={POLICY}", "--model=unknown-model"}},
		runcase{Label: "no model flag", Args: []string{"--json", "route", "identity", "--policy={POLICY}"}},
		runcase{Label: "missing policy", Args: []string{"--json", "route", "identity", "--policy={ROOT}/none.yaml", "--model=opus"}},
	)
	s.runAll("route-identity", cases)
}

func TestRouteUnknownSubcommand(t *testing.T) {
	s := newSandbox(t)
	cases := []runcase{
		{Label: "unknown", Args: []string{"route", "nonsense"}},
		{Label: "no subcommand", Args: []string{"route"}},
		{Label: "dispatch without selector", Args: []string{"--json", "route", "dispatch", "--policy={POLICY}"}},
	}
	s.runAll("route-unknown", cases)
}
