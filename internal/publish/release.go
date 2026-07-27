package publish

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	releaseVerifyScript = "verify-release-binaries.sh"
	releaseBuildScript  = "build-release.sh"

	// releaseUnavailableExit is the reserved exit code a release script uses to
	// say "I could not reach a verdict" — a missing dependency, an unresolvable
	// module replacement, anything about the MACHINE rather than the artifacts.
	//
	// An exit code rather than pattern-matching the output on purpose. Matching
	// strings like "is required" would make the distinction depend on wording
	// nobody is watching: reword one `die` message and the script silently goes
	// back to reporting stale, which is precisely the regression this exists to
	// prevent. 3 is outside bash's reserved range (1 general, 2 misuse, 126 not
	// executable, 127 not found), so it can only be set deliberately.
	releaseUnavailableExit = 3
)

// prepareReleaseArtifacts enforces the conventional release-script contract.
// Plugins without a verifier are unmanaged and pass through unchanged. A
// normal version publish may rebuild stale artifacts; dry-run and sync-only
// paths pass allowBuild=false and therefore fail without mutating the plugin.
//
// A verifier that could not RUN never reaches the rebuild path. Rebuilding
// after an unavailable verdict is how the original bug compounded: the builder
// failed for the identical missing dependency, and the operator was told the
// artifacts were stale twice over by a machine that had never inspected them.
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
	output, err := runReleaseScript(pluginRoot, buildPath)
	if err != nil {
		// The builder speaks the same protocol. A build that could not run is
		// not evidence the artifacts are stale.
		if unavailable, why := classifyScriptFailure(output, err); unavailable {
			return nil, false, unavailableErr("release builder", why)
		}
		return nil, false, fmt.Errorf("%w: release build failed: %s", ErrStaleReleaseArtifacts, scriptFailure(output, err))
	}

	if err := verifyReleaseArtifacts(pluginRoot); err != nil {
		if errors.Is(err, ErrReleaseVerifierUnavailable) {
			return nil, false, err
		}
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
//
// Three outcomes, deliberately distinct:
//
//	nil                            — the verifier ran and asserted the artifacts match
//	ErrReleaseVerifierUnavailable  — the verifier could not reach a verdict
//	ErrStaleReleaseArtifacts       — the verifier ran and found a mismatch
func verifyReleaseArtifacts(pluginRoot string) error {
	verifyPath := filepath.Join(pluginRoot, "scripts", releaseVerifyScript)
	if _, err := os.Stat(verifyPath); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat release verifier: %w", err)
	}
	output, err := runReleaseScript(pluginRoot, verifyPath)
	if err != nil {
		if unavailable, why := classifyScriptFailure(output, err); unavailable {
			return unavailableErr("release verifier", why)
		}
		return fmt.Errorf("%w: %s", ErrStaleReleaseArtifacts, scriptFailure(output, err))
	}
	// Exit 0 is not the same as "verified". A verifier that returns early —
	// an added guard clause, a short-circuit for one platform — exits 0 having
	// checked nothing, and silence would read as a pass. That direction is
	// worse than the one this file was opened to fix, because it publishes
	// unverified binaries instead of merely refusing to publish good ones.
	// The receipt is the assertion; without it there is no verdict to trust.
	if !hasVerificationReceipt(output) {
		return unavailableErr("release verifier", "exited 0 without a verification receipt; expected a JSON line with \"verified\":true")
	}
	return nil
}

// hasVerificationReceipt looks for the verifier's success receipt anywhere in
// its output. Scanning line by line rather than parsing the whole stream
// because output is captured combined, so stderr diagnostics may surround it.
func hasVerificationReceipt(output string) bool {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var receipt struct {
			Verified bool `json:"verified"`
		}
		if json.Unmarshal([]byte(line), &receipt) == nil && receipt.Verified {
			return true
		}
	}
	return false
}

// classifyScriptFailure decides whether a failed release script was unable to
// run at all. Reports the reason to surface alongside it.
func classifyScriptFailure(output string, err error) (bool, string) {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if exitErr.ExitCode() == releaseUnavailableExit {
			return true, scriptFailure(output, err)
		}
		return false, ""
	}
	// Not an ExitError at all: bash itself could not be started, the script is
	// not executable, the working directory vanished. Nothing was inspected.
	return true, scriptFailure(output, err)
}

// unavailableErr names the machine. "go is required" is unactionable without
// it — the whole failure mode is one machine being unable to check something
// another machine checks fine, and an operator reading a log elsewhere has no
// way to tell which host produced the line.
func unavailableErr(what, why string) error {
	host, hostErr := os.Hostname()
	if hostErr != nil || host == "" {
		host = "unknown host"
	}
	return fmt.Errorf("%w: %s on %s: %s", ErrReleaseVerifierUnavailable, what, host, why)
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
