package publish

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// GitStatus checks if a git worktree is clean, including untracked files.
// Ignored build outputs do not make a repository dirty.
func GitStatus(dir string) (clean bool, err error) {
	cmd := exec.Command("git", "-C", dir, "status", "--porcelain=v1", "--untracked-files=normal")
	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("git status: %w", err)
	}
	return len(bytes.TrimSpace(out)) == 0, nil
}

// GitShowFile returns a tracked file's contents as committed at HEAD.
// relPath is relative to the repository root, not to the caller's directory.
func GitShowFile(dir, relPath string) ([]byte, error) {
	cmd := exec.Command("git", "-C", dir, "show", "HEAD:"+relPath)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git show HEAD:%s: %s: %w",
			relPath, strings.TrimSpace(stderr.String()), err)
	}
	return out.Bytes(), nil
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
//
// A failed pull aborts whatever rebase it started, so no caller inherits a
// repository wedged mid-rebase (mk-pn74). This used to return the error and
// walk away, leaving rebase state on disk that blocked every subsequent
// publish until a human repaired the clone by hand -- and the operator had no
// way to tell from the message that the tree had been left in that state.
//
// The error therefore says which happened. "The pull failed" and "the pull
// failed and your repository is now mid-rebase" call for different responses,
// so they must not read identically.
func GitPullRebase(dir string) error {
	cmd := exec.Command("git", "-C", dir, "pull", "--rebase")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git pull --rebase: %s: %w%s",
			strings.TrimSpace(stderr.String()), err, abortAnyRebase(dir))
	}
	return nil
}

// abortAnyRebase restores dir when a rebase is in progress, returning a note to
// append to the caller's error. It returns "" when there was nothing to abort,
// so a pull that failed before starting a rebase -- no upstream, unreachable
// remote, dirty tree -- never claims to have aborted one.
func abortAnyRebase(dir string) string {
	if !rebaseInProgress(dir) {
		return ""
	}
	cmd := exec.Command("git", "-C", dir, "rebase", "--abort")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Sprintf(" [WARNING: %s is STILL mid-rebase, `git rebase --abort` failed: %s]",
			dir, strings.TrimSpace(stderr.String()))
	}
	return fmt.Sprintf(" [rebase aborted; %s restored to its pre-pull state]", dir)
}

// rebaseInProgress reports whether dir has an interrupted rebase.
//
// Detection goes through `git rev-parse --git-path` rather than a hardcoded
// .git/rebase-merge: inside a worktree .git is a FILE, and the rebase state
// actually lives under the parent repository's .git/worktrees/<name>/. Guessing
// the path would silently report "no rebase" for every worktree, which is the
// same could-not-look-reported-as-not-there mistake this whole bead is about.
func rebaseInProgress(dir string) bool {
	for _, name := range []string{"rebase-merge", "rebase-apply"} {
		cmd := exec.Command("git", "-C", dir, "rev-parse", "--git-path", name)
		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = nil
		if err := cmd.Run(); err != nil {
			continue
		}
		p := strings.TrimSpace(out.String())
		if p == "" {
			continue
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
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

// GitRemoteURL returns the URL of the origin remote.
func GitRemoteURL(dir string) (string, error) {
	cmd := exec.Command("git", "-C", dir, "remote", "get-url", "origin")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git remote get-url origin: %w", err)
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
