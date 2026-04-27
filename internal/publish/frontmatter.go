package publish

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ValidateFrontmatter walks a plugin's skill and command markdown files and
// asserts that each one has a YAML frontmatter block parseable by yaml.v3.
//
// Motivation: clavain 0.6.245 shipped commands/repro-first-debugging.md with
// an unquoted colon in the description ("investigation: reproduce..."). YAML
// parsed it as a mapping, the harness silently dropped the entire plugin
// from skill_listing, and 68 entries vanished from a "−1KB trim" PR. See
// sylveste-ulp8 for the full incident report.
//
// The check is deliberately strict: any unparseable frontmatter aborts the
// publish, even if the file contains other valid YAML. The skill_listing
// path treats the same condition as silent failure, so the publish gate is
// the only place a broken description gets caught before users see it.
func ValidateFrontmatter(pluginRoot string) error {
	var bad []string

	walkDirs := []struct {
		dir     string
		pattern string
	}{
		{filepath.Join(pluginRoot, "skills"), "SKILL.md"},
		{filepath.Join(pluginRoot, "commands"), "*.md"},
	}

	for _, w := range walkDirs {
		if _, err := os.Stat(w.dir); os.IsNotExist(err) {
			continue
		}

		err := filepath.WalkDir(w.dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if w.pattern == "SKILL.md" && filepath.Base(path) != "SKILL.md" {
				return nil
			}
			if w.pattern == "*.md" && !strings.HasSuffix(path, ".md") {
				return nil
			}

			if msg := checkFrontmatter(path); msg != "" {
				rel, relErr := filepath.Rel(pluginRoot, path)
				if relErr != nil {
					rel = path
				}
				bad = append(bad, fmt.Sprintf("  %s: %s", rel, msg))
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("walk %s: %w", w.dir, err)
		}
	}

	if len(bad) > 0 {
		return fmt.Errorf("frontmatter parse errors in %d file(s):\n%s",
			len(bad), strings.Join(bad, "\n"))
	}
	return nil
}

// checkFrontmatter returns "" if the file has parseable YAML frontmatter or
// no frontmatter at all (some commands are pure prompts). Returns a diagnostic
// string if the frontmatter exists but fails to parse.
func checkFrontmatter(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("read: %v", err)
	}

	text := string(data)
	if !strings.HasPrefix(text, "---\n") && !strings.HasPrefix(text, "---\r\n") {
		return ""
	}

	// Strip the leading "---" line then find the closing "---".
	rest := text[4:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "missing closing '---' delimiter"
	}

	body := rest[:end]
	var parsed map[string]interface{}
	if err := yaml.Unmarshal([]byte(body), &parsed); err != nil {
		return fmt.Sprintf("yaml: %v", err)
	}
	if _, ok := parsed["description"]; !ok {
		// Every Claude Code skill/command needs a description for the
		// harness to render a listing entry. Missing description is the
		// same class of failure as the YAML error — silent invisibility.
		return "missing 'description' field"
	}
	return ""
}
