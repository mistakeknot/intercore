package publish

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

// GitStatus checks if a git worktree is clean (no staged or unstaged changes).
func GitStatus(dir string) (clean bool, err error) {
	// --quiet exits 1 if there are changes
	staged := exec.Command("git", "-C", dir, "diff", "--cached", "--quiet")
	unstaged := exec.Command("git", "-C", dir, "diff", "--quiet")

	if err := staged.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return false, nil
		}
		return false, fmt.Errorf("git diff --cached: %w", err)
	}
	if err := unstaged.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return false, nil
		}
		return false, fmt.Errorf("git diff: %w", err)
	}

	// Also check for untracked files that might be staged
	return true, nil
}

// GitRemoteReachable checks if the origin remote is accessible.
func GitRemoteReachable(dir string) error {
	cmd := exec.Command("git", "-C", dir, "ls-remote", "--exit-code", "origin", "HEAD")
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: origin not reachable from %s", ErrRemoteUnreachable, dir)
	}
	return nil
}

// GitDirtyFiles returns paths of modified (unstaged) files relative to the repo root.
func GitDirtyFiles(dir string) ([]string, error) {
	cmd := exec.Command("git", "-C", dir, "diff", "--name-only")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git diff --name-only: %w", err)
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}

// GitAdd stages specific files relative to the repo root.
func GitAdd(dir string, files ...string) error {
	args := append([]string{"-C", dir, "add"}, files...)
	cmd := exec.Command("git", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git add: %s: %w", stderr.String(), err)
	}
	return nil
}

// GitCommit creates a commit with the given message.
func GitCommit(dir, message string) error {
	cmd := exec.Command("git", "-C", dir, "commit", "-m", message)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git commit: %s: %w", stderr.String(), err)
	}
	return nil
}

// GitPullRebase runs git pull --rebase to sync with remote.
func GitPullRebase(dir string) error {
	cmd := exec.Command("git", "-C", dir, "pull", "--rebase")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git pull --rebase: %s: %w", stderr.String(), err)
	}
	return nil
}

// GitPush pushes to origin. Never forces, never amends.
func GitPush(dir string) error {
	cmd := exec.Command("git", "-C", dir, "push")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git push: %s: %w", stderr.String(), err)
	}
	return nil
}

// GitHeadCommit returns the current HEAD commit SHA.
func GitHeadCommit(dir string) (string, error) {
	cmd := exec.Command("git", "-C", dir, "rev-parse", "HEAD")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	return strings.TrimSpace(out.String()), nil
}

// GitRevert creates revert commits for the last n commits.
func GitRevert(dir string, n int) error {
	for i := 0; i < n; i++ {
		ref := fmt.Sprintf("HEAD~%d", i)
		cmd := exec.Command("git", "-C", dir, "revert", "--no-edit", ref)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("git revert %s: %s: %w", ref, stderr.String(), err)
		}
	}
	return nil
}

// GitTopLevel returns the repository root directory.
func GitTopLevel(dir string) (string, error) {
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git rev-parse --show-toplevel: %w", err)
	}
	return strings.TrimSpace(out.String()), nil
}

// execCommand wraps exec.Command for testability.
var execCommand = exec.Command
