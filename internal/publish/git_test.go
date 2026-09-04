package publish

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitStatusRejectsUntrackedButAllowsIgnoredFiles(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("ignored.bin\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("tracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "init")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-m", "init")

	if clean, err := GitStatus(repo); err != nil || !clean {
		t.Fatalf("fresh repository clean = %v, err = %v", clean, err)
	}
	if err := os.WriteFile(filepath.Join(repo, "ignored.bin"), []byte("ignored\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if clean, err := GitStatus(repo); err != nil || !clean {
		t.Fatalf("repository with ignored output clean = %v, err = %v", clean, err)
	}
	if err := os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("untracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if clean, err := GitStatus(repo); err != nil || clean {
		t.Fatalf("repository with untracked file clean = %v, err = %v", clean, err)
	}
}

// --- shared git fixtures -----------------------------------------------------
//
// These build REAL repositories with REAL remotes. Push and rebase behaviour is
// the whole subject of mk-pn74, and neither can be exercised against a bare
// temp directory: git simply fails there, which is indistinguishable from the
// failure the tests are supposed to detect.

// gitOutput runs a git command and returns its trimmed stdout.
func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Claude", "GIT_AUTHOR_EMAIL=noreply@anthropic.com",
		"GIT_COMMITTER_NAME=Claude", "GIT_COMMITTER_EMAIL=noreply@anthropic.com",
	)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v in %s: %v", args, dir, err)
	}
	return strings.TrimSpace(string(out))
}

func writeIn(t *testing.T, dir, name, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

// gitIdentity gives a repository its own committer identity.
//
// Production GitCommit shells out to a plain `git commit` with no GIT_AUTHOR_*
// environment, unlike the runGit helper. A fixture that leans on the
// developer's global config therefore passes locally -- macOS git happily
// derives an identity from user and hostname -- and fails on a CI runner with
// "Author identity unknown". The repo has to carry its own.
func gitIdentity(t *testing.T, dir string) {
	t.Helper()
	runGit(t, dir, "config", "user.email", "noreply@anthropic.com")
	runGit(t, dir, "config", "user.name", "Claude")
}

// newBareOrigin creates an empty bare repository suitable as a push target.
func newBareOrigin(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	origin := filepath.Join(base, "origin.git")
	runGit(t, base, "init", "--bare", "-b", "main", origin)
	return origin
}

// cloneOf clones origin into a fresh directory.
func cloneOf(t *testing.T, origin string) string {
	t.Helper()
	base := t.TempDir()
	dir := filepath.Join(base, "clone")
	runGit(t, base, "clone", origin, dir)
	gitIdentity(t, dir)
	return dir
}

// seededOrigin returns a bare origin holding one commit of `name`, plus a clone
// of it positioned on main with an upstream.
func seededOrigin(t *testing.T, name, contents string) (origin, clone string) {
	t.Helper()
	origin = newBareOrigin(t)
	clone = cloneOf(t, origin)
	writeIn(t, clone, name, contents)
	runGit(t, clone, "add", name)
	runGit(t, clone, "commit", "-m", "seed")
	runGit(t, clone, "push", "-u", "origin", "main")
	return origin, clone
}

// --- mk-pn74 (b): a failed pull must not leave the repo wedged ---------------

// The wedge: a conflicted `pull --rebase` used to return its error and leave
// rebase state on disk, so the NEXT publish from that clone died before it
// started and only a human could clear it. That is how the 0.6.300 publish
// ended up half-applied -- plugin pushed, marketplace not.
func TestGitPullRebase_ConflictLeavesNoRebaseInProgress(t *testing.T) {
	origin, a := seededOrigin(t, "version.txt", "0.6.298\n")

	// Another machine moves the line first.
	b := cloneOf(t, origin)
	writeIn(t, b, "version.txt", "0.6.299\n")
	runGit(t, b, "commit", "-am", "remote bump")
	runGit(t, b, "push", "origin", "main")

	// We move the same line locally, then sync -- a guaranteed same-line conflict.
	writeIn(t, a, "version.txt", "0.6.300\n")
	runGit(t, a, "commit", "-am", "local bump")

	err := GitPullRebase(a)
	if err == nil {
		t.Fatal("a conflicting pull --rebase reported success")
	}
	if rebaseInProgress(a) {
		t.Fatal("repository left mid-rebase -- the mk-pn74 wedge is back")
	}
	if !strings.Contains(err.Error(), "rebase aborted") {
		t.Errorf("error never tells the operator the tree was restored: %v", err)
	}

	// "Restored" has to mean USABLE, not merely that the rebase directory is
	// gone: the local commit is still HEAD and the worktree is clean.
	if got := gitOutput(t, a, "log", "-1", "--pretty=%s"); got != "local bump" {
		t.Errorf("HEAD subject = %q, want the local commit back", got)
	}
	if clean, err := GitStatus(a); err != nil || !clean {
		t.Errorf("worktree not clean after abort: clean=%v err=%v", clean, err)
	}
}

// Control. Without this, the test above would still pass if GitPullRebase
// simply failed at everything.
func TestGitPullRebase_CleanSyncStillSucceeds(t *testing.T) {
	origin, a := seededOrigin(t, "version.txt", "0.6.298\n")

	b := cloneOf(t, origin)
	writeIn(t, b, "version.txt", "0.6.299\n")
	runGit(t, b, "commit", "-am", "remote bump")
	runGit(t, b, "push", "origin", "main")

	if err := GitPullRebase(a); err != nil {
		t.Fatalf("fast-forwardable pull: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(a, "version.txt"))
	if err != nil || string(got) != "0.6.299\n" {
		t.Errorf("clone did not take the remote change: %q (%v)", got, err)
	}
}

// A pull can fail without ever starting a rebase -- no upstream, unreachable
// remote, dirty tree. Reporting an abort that never happened would be a false
// reassurance the operator acts on.
func TestGitPullRebase_FailureWithoutRebaseDoesNotClaimAbort(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init", "-b", "main")
	writeIn(t, repo, "f.txt", "x\n")
	runGit(t, repo, "add", "f.txt")
	runGit(t, repo, "commit", "-m", "only commit")

	err := GitPullRebase(repo) // no remote configured at all
	if err == nil {
		t.Fatal("pull with no remote reported success")
	}
	if strings.Contains(err.Error(), "rebase aborted") {
		t.Errorf("claimed to abort a rebase that never started: %v", err)
	}
}
