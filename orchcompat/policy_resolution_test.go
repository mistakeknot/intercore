package orchcompat

import (
	"os"
	"path/filepath"
	"testing"
)

func (s *sandbox) copyPolicy(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(s.policy)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func (s *sandbox) link(t *testing.T, target, rel string) {
	t.Helper()
	path := filepath.Join(s.root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}

func TestPolicyResolutionPrecedence(t *testing.T) {
	s := newSandbox(t)
	s.copyPolicy(t, "sel/flag/routing.yaml")
	s.copyPolicy(t, "sel/env-policy/routing.yaml")
	s.copyPolicy(t, "sel/env-root/config/routing.yaml")
	s.copyPolicy(t, "sel/plugin-root/config/routing.yaml")
	s.copyPolicy(t, "install/config/routing.yaml")
	s.copyPolicy(t, "install/skills/using-clavain/SKILL.md")
	s.link(t, filepath.Join(s.root, "install", "skills", "using-clavain", "SKILL.md"), "home/.agents/skills/clavain/using-clavain/SKILL.md")
	s.copyPolicy(t, "trees/plain/config/routing.yaml")
	s.copyPolicy(t, "trees/upper/os/Clavain/config/routing.yaml")
	s.copyPolicy(t, "trees/lower/os/clavain/config/routing.yaml")
	s.copyPolicy(t, "trees/both/config/routing.yaml")
	s.copyPolicy(t, "trees/both/os/Clavain/config/routing.yaml")
	s.copyPolicy(t, "trees/nested/config/routing.yaml")
	s.copyPolicy(t, "trees/nested/inner/config/routing.yaml")
	s.link(t, filepath.Join(s.root, "sel", "flag", "routing.yaml"), "links/policy-link.yaml")
	s.link(t, filepath.Join(s.root, "sel", "absent.yaml"), "links/dangling.yaml")
	for _, d := range []string{"trees/plain/deep/er", "trees/upper/x", "trees/lower/x", "trees/both/x", "trees/nested/inner/x", "trees/empty/x"} {
		if err := os.MkdirAll(filepath.Join(s.root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	args := []string{"--json", "route", "dispatch", "--role=planning"}
	flagArgs := []string{"--json", "route", "dispatch", "--role=planning", "--policy={ROOT}/sel/flag/routing.yaml"}
	noHome := map[string]string{"HOME": "{ROOT}/home-empty"}
	if err := os.MkdirAll(filepath.Join(s.root, "home-empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []runcase{
		{Label: "flag alone", Args: flagArgs},
		{Label: "flag beats every environment selection", Args: flagArgs, Env: map[string]string{
			"CLAVAIN_ROUTING_POLICY": "{ROOT}/sel/env-policy/routing.yaml",
			"CLAVAIN_ROOT":           "{ROOT}/sel/env-root",
			"CLAUDE_PLUGIN_ROOT":     "{ROOT}/sel/plugin-root",
		}},
		{Label: "routing policy env beats roots", Args: args, Env: map[string]string{
			"CLAVAIN_ROUTING_POLICY": "{ROOT}/sel/env-policy/routing.yaml",
			"CLAVAIN_ROOT":           "{ROOT}/sel/env-root",
			"CLAUDE_PLUGIN_ROOT":     "{ROOT}/sel/plugin-root",
		}},
		{Label: "clavain root beats plugin root", Args: args, Env: map[string]string{
			"CLAVAIN_ROOT":       "{ROOT}/sel/env-root",
			"CLAUDE_PLUGIN_ROOT": "{ROOT}/sel/plugin-root",
		}},
		{Label: "plugin root with policy file", Args: args, Env: map[string]string{"CLAUDE_PLUGIN_ROOT": "{ROOT}/sel/plugin-root"}},
		{Label: "plugin root without policy file falls through to home link", Args: args, Env: map[string]string{"CLAUDE_PLUGIN_ROOT": "{ROOT}/sel/no-such-root"}},
		{Label: "plugin root without policy file and no home", Args: args, Env: map[string]string{"CLAUDE_PLUGIN_ROOT": "{ROOT}/sel/no-such-root", "HOME": "{ROOT}/home-empty"}},
		{Label: "clavain root missing policy fails closed", Args: args, Env: map[string]string{"CLAVAIN_ROOT": "{ROOT}/sel/no-such-root", "CLAUDE_PLUGIN_ROOT": "{ROOT}/sel/plugin-root"}},
		{Label: "routing policy env missing fails closed", Args: args, Env: map[string]string{"CLAVAIN_ROUTING_POLICY": "{ROOT}/sel/absent.yaml", "CLAVAIN_ROOT": "{ROOT}/sel/env-root"}},
		{Label: "routing policy env is a directory", Args: args, Env: map[string]string{"CLAVAIN_ROUTING_POLICY": "{ROOT}/sel"}},
		{Label: "routing policy env dangling link", Args: args, Env: map[string]string{"CLAVAIN_ROUTING_POLICY": "{ROOT}/links/dangling.yaml"}},
		{Label: "flag dangling link", Args: []string{"--json", "route", "dispatch", "--role=planning", "--policy={ROOT}/links/dangling.yaml"}},
		{Label: "flag symlink reports target", Args: []string{"--json", "route", "dispatch", "--role=planning", "--policy={ROOT}/links/policy-link.yaml"}},
		{Label: "home skill link", Args: args},
		{Label: "home skill link beats cwd walk", Args: args, Cwd: "{ROOT}/trees/plain/deep/er"},
		{Label: "env beats home skill link", Args: args, Env: map[string]string{"CLAVAIN_ROOT": "{ROOT}/sel/env-root"}},
		{Label: "cwd walk finds config", Args: args, Cwd: "{ROOT}/trees/plain/deep/er", Env: noHome},
		{Label: "cwd walk finds os/Clavain", Args: args, Cwd: "{ROOT}/trees/upper/x", Env: noHome},
		{Label: "cwd walk finds os/clavain", Args: args, Cwd: "{ROOT}/trees/lower/x", Env: noHome},
		{Label: "cwd walk prefers config over os/Clavain", Args: args, Cwd: "{ROOT}/trees/both/x", Env: noHome},
		{Label: "cwd walk prefers nearest ancestor", Args: args, Cwd: "{ROOT}/trees/nested/inner/x", Env: noHome},
		{Label: "cwd walk finds nothing", Args: args, Cwd: "{ROOT}/trees/empty/x", Env: noHome},
		{Label: "home link dangling falls to cwd walk", Args: args, Cwd: "{ROOT}/trees/plain/deep/er", Env: map[string]string{"HOME": "{ROOT}/home-dangling"}},
		{Label: "text output uses same resolution", Args: []string{"route", "dispatch", "--role=planning"}, Env: map[string]string{"CLAVAIN_ROOT": "{ROOT}/sel/env-root"}},
		{Label: "route model uses same resolution", Args: []string{"--json", "route", "model", "--phase=planned", "--category=review", "--agent=fd-safety"}, Env: map[string]string{"CLAVAIN_ROOT": "{ROOT}/sel/env-root"}},
		{Label: "route table without any selection", Args: []string{"--json", "route", "table"}, Env: noHome},
	}
	dangling := filepath.Join(s.root, "home-dangling", ".agents", "skills", "clavain", "using-clavain")
	if err := os.MkdirAll(dangling, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(s.root, "nowhere", "SKILL.md"), filepath.Join(dangling, "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	s.runAll("policy-resolution", cases)
}
