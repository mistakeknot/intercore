package publish

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	releaseVerifyScript = "verify-release-binaries.sh"
	releaseBuildScript  = "build-release.sh"
)

// prepareReleaseArtifacts enforces the conventional release-script contract.
// Plugins without a verifier are unmanaged and pass through unchanged. A
// normal version publish may rebuild stale artifacts; dry-run and sync-only
// paths pass allowBuild=false and therefore fail without mutating the plugin.
func prepareReleaseArtifacts(pluginRoot string, allowBuild bool) ([]string, bool, error) {
	verifyErr := verifyReleaseArtifacts(pluginRoot)
	if verifyErr == nil {
		return nil, false, nil
	}
	if !errors.Is(verifyErr, ErrStaleReleaseArtifacts) {
		return nil, false, verifyErr
	}
	if !allowBuild {
		return nil, false, verifyErr
	}

	buildPath := filepath.Join(pluginRoot, "scripts", releaseBuildScript)
	if _, err := os.Stat(buildPath); err != nil {
		if os.IsNotExist(err) {
			return nil, false, fmt.Errorf("%w: %v; scripts/%s is missing", ErrStaleReleaseArtifacts, verifyErr, releaseBuildScript)
		}
		return nil, false, fmt.Errorf("stat release builder: %w", err)
	}
	if output, err := runReleaseScript(pluginRoot, buildPath); err != nil {
		return nil, false, fmt.Errorf("%w: release build failed: %s", ErrStaleReleaseArtifacts, scriptFailure(output, err))
	}

	if err := verifyReleaseArtifacts(pluginRoot); err != nil {
		return nil, false, fmt.Errorf("%w: rebuilt artifacts did not verify: %v", ErrStaleReleaseArtifacts, err)
	}
	files, err := GitDirtyFiles(pluginRoot)
	if err != nil {
		return nil, false, fmt.Errorf("list rebuilt release artifacts: %w", err)
	}
	if len(files) == 0 {
		return nil, false, fmt.Errorf("%w: release builder produced no tracked changes", ErrStaleReleaseArtifacts)
	}
	return files, true, nil
}

// verifyReleaseArtifacts runs the plugin's verifier when one is present.
// Absence means the plugin does not opt into managed release artifacts.
func verifyReleaseArtifacts(pluginRoot string) error {
	verifyPath := filepath.Join(pluginRoot, "scripts", releaseVerifyScript)
	if _, err := os.Stat(verifyPath); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat release verifier: %w", err)
	}
	if output, err := runReleaseScript(pluginRoot, verifyPath); err != nil {
		return fmt.Errorf("%w: %s", ErrStaleReleaseArtifacts, scriptFailure(output, err))
	}
	return nil
}

func runReleaseScript(pluginRoot, script string) (string, error) {
	cmd := execCommand("bash", script)
	cmd.Dir = pluginRoot
	output, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(output)), err
}

func scriptFailure(output string, err error) string {
	if output != "" {
		return output
	}
	return err.Error()
}
