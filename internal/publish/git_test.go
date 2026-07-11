package publish

import (
	"os"
	"path/filepath"
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
