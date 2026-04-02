package publish

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// FindInterchart locates the interchart plugin directory by walking up from pluginRoot
// to find the interverse parent, then checking for interchart/scripts/generate.sh.
func FindInterchart(pluginRoot string) (string, error) {
	// Walk up to find the interverse directory
	dir := pluginRoot
	for i := 0; i < 5; i++ {
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		// Check if parent contains interchart
		candidate := filepath.Join(parent, "interchart", "scripts", "generate.sh")
		if _, err := os.Stat(candidate); err == nil {
			return filepath.Join(parent, "interchart"), nil
		}
		dir = parent
	}
	return "", fmt.Errorf("interchart not found")
}

// RegenerateInterchart runs the interchart scanner and regenerates the ecosystem diagram.
// The monorepo root is derived by walking up from pluginRoot to find .git.
func RegenerateInterchart(interchartRoot, pluginRoot string) error {
	// Find monorepo root (nearest .git ancestor of interchartRoot)
	monorepoRoot := interchartRoot
	for i := 0; i < 5; i++ {
		if _, err := os.Stat(filepath.Join(monorepoRoot, ".git")); err == nil {
			break
		}
		monorepoRoot = filepath.Dir(monorepoRoot)
	}

	generateSh := filepath.Join(interchartRoot, "scripts", "generate.sh")
	cmd := exec.Command("bash", generateSh, monorepoRoot)
	cmd.Dir = interchartRoot
	cmd.Stdout = nil // suppress output — we only care about errors
	cmd.Stderr = nil

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("generate.sh: %w", err)
	}

	// Commit and push if there are changes
	scanJSON := filepath.Join("data", "scan.json")
	if err := GitAdd(interchartRoot, scanJSON); err != nil {
		return nil // no changes — that's fine
	}

	commitMsg := "chore: regenerate ecosystem diagram (post-publish)"
	if err := GitCommit(interchartRoot, commitMsg); err != nil {
		return nil // nothing to commit
	}

	if err := GitPush(interchartRoot); err != nil {
		return fmt.Errorf("push interchart: %w", err)
	}

	return nil
}
