package publish

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeMD(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestValidateFrontmatter_Clean(t *testing.T) {
	dir := t.TempDir()
	writeMD(t, filepath.Join(dir, "skills", "good-skill", "SKILL.md"), `---
name: good-skill
description: A clean description with no colons or weirdness.
---

# Body
`)
	writeMD(t, filepath.Join(dir, "commands", "ok-cmd.md"), `---
name: ok-cmd
description: Another fine description.
---
`)

	if err := ValidateFrontmatter(dir); err != nil {
		t.Fatalf("expected pass, got: %v", err)
	}
}

func TestValidateFrontmatter_UnquotedColon(t *testing.T) {
	// The exact regression that took out clavain in 0.6.245.
	dir := t.TempDir()
	writeMD(t, filepath.Join(dir, "commands", "repro.md"), `---
name: repro
description: Disciplined bug investigation: reproduce first, then diagnose.
---
`)

	err := ValidateFrontmatter(dir)
	if err == nil {
		t.Fatal("expected YAML error for unquoted colon, got nil")
	}
	if !strings.Contains(err.Error(), "repro.md") {
		t.Errorf("error should name the offending file, got: %v", err)
	}
	if !strings.Contains(err.Error(), "yaml") {
		t.Errorf("error should identify YAML parse failure, got: %v", err)
	}
}

func TestValidateFrontmatter_QuotedColonOK(t *testing.T) {
	// Same content but quoted — should pass.
	dir := t.TempDir()
	writeMD(t, filepath.Join(dir, "commands", "repro.md"), `---
name: repro
description: "Disciplined bug investigation: reproduce first, then diagnose."
---
`)

	if err := ValidateFrontmatter(dir); err != nil {
		t.Fatalf("quoted colon should be fine, got: %v", err)
	}
}

func TestValidateFrontmatter_EmDashOK(t *testing.T) {
	// The fix shipped in clavain 0.6.247.
	dir := t.TempDir()
	writeMD(t, filepath.Join(dir, "commands", "repro.md"), `---
name: repro
description: Disciplined bug investigation — reproduce first, then diagnose.
---
`)

	if err := ValidateFrontmatter(dir); err != nil {
		t.Fatalf("em-dash variant should pass, got: %v", err)
	}
}

func TestValidateFrontmatter_MissingDescription(t *testing.T) {
	dir := t.TempDir()
	writeMD(t, filepath.Join(dir, "skills", "lazy", "SKILL.md"), `---
name: lazy
---

# Body
`)

	err := ValidateFrontmatter(dir)
	if err == nil {
		t.Fatal("expected error for missing description")
	}
	if !strings.Contains(err.Error(), "description") {
		t.Errorf("error should mention 'description', got: %v", err)
	}
}

func TestValidateFrontmatter_NoFrontmatterIgnored(t *testing.T) {
	// Some command files are pure prompts with no frontmatter — these
	// won't appear in skill_listing anyway, so don't fail the publish on
	// them.
	dir := t.TempDir()
	writeMD(t, filepath.Join(dir, "commands", "raw-prompt.md"), `# Just a heading

Some prose with no YAML.
`)

	if err := ValidateFrontmatter(dir); err != nil {
		t.Fatalf("file without frontmatter should be ignored, got: %v", err)
	}
}

func TestValidateFrontmatter_UnclosedFrontmatter(t *testing.T) {
	dir := t.TempDir()
	writeMD(t, filepath.Join(dir, "commands", "broken.md"), `---
name: broken
description: starts but never closes
`)

	err := ValidateFrontmatter(dir)
	if err == nil {
		t.Fatal("expected error for unclosed frontmatter")
	}
	if !strings.Contains(err.Error(), "closing") {
		t.Errorf("error should mention closing delimiter, got: %v", err)
	}
}

func TestValidateFrontmatter_NoDirsToleratedSilently(t *testing.T) {
	// Plugins with no skills/ or commands/ dir (e.g. agent-only) should pass.
	dir := t.TempDir()
	if err := ValidateFrontmatter(dir); err != nil {
		t.Fatalf("plugin with no skill/command dirs should pass, got: %v", err)
	}
}

func TestValidateFrontmatter_SkillsButNoSKILLmdIgnored(t *testing.T) {
	// Random files inside skills/ shouldn't be picked up — only SKILL.md.
	dir := t.TempDir()
	writeMD(t, filepath.Join(dir, "skills", "good", "SKILL.md"), `---
name: good
description: Fine.
---
`)
	writeMD(t, filepath.Join(dir, "skills", "good", "notes.md"), `---
busted: yaml: with: colons
---
`)

	if err := ValidateFrontmatter(dir); err != nil {
		t.Fatalf("non-SKILL.md files in skills/ should be ignored, got: %v", err)
	}
}

func TestValidateFrontmatter_AggregatesErrors(t *testing.T) {
	// Multiple bad files should all surface in one error.
	dir := t.TempDir()
	writeMD(t, filepath.Join(dir, "commands", "bad1.md"), `---
description: this has: a colon
---
`)
	writeMD(t, filepath.Join(dir, "commands", "bad2.md"), `---
description: this also: has one
---
`)

	err := ValidateFrontmatter(dir)
	if err == nil {
		t.Fatal("expected aggregated error")
	}
	if !strings.Contains(err.Error(), "bad1.md") || !strings.Contains(err.Error(), "bad2.md") {
		t.Errorf("expected both filenames in error, got: %v", err)
	}
}
