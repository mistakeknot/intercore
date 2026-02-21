package dispatch

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// initGitRepo creates a temp git repo with an initial commit.
func initGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	cmds := [][]string{
		{"git", "-C", dir, "init"},
		{"git", "-C", dir, "config", "user.email", "test@test.com"},
		{"git", "-C", dir, "config", "user.name", "Test"},
	}
	for _, args := range cmds {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("git setup %v: %v\n%s", args, err, out)
		}
	}

	// Create initial file and commit
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cmds = [][]string{
		{"git", "-C", dir, "add", "."},
		{"git", "-C", dir, "commit", "-m", "initial"},
	}
	for _, args := range cmds {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("git commit %v: %v\n%s", args, err, out)
		}
	}

	return dir
}

func TestCheckWriteSetConflict_NoBaseCommit(t *testing.T) {
	result, err := CheckWriteSetConflict("/tmp", "", []string{"foo.go"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.HasConflict {
		t.Error("expected no conflict when base commit is empty")
	}
}

func TestCheckWriteSetConflict_NoDivergence(t *testing.T) {
	dir := initGitRepo(t)
	head, err := gitHeadCommit(dir)
	if err != nil {
		t.Fatal(err)
	}

	result, err := CheckWriteSetConflict(dir, head, []string{"foo.go"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.HasConflict {
		t.Error("expected no conflict when HEAD == base")
	}
}

func TestCheckWriteSetConflict_NoOverlap(t *testing.T) {
	dir := initGitRepo(t)
	base, _ := gitHeadCommit(dir)

	// Make a concurrent change to a different file
	if err := os.WriteFile(filepath.Join(dir, "other.go"), []byte("package other\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cmds := [][]string{
		{"git", "-C", dir, "add", "other.go"},
		{"git", "-C", dir, "commit", "-m", "add other"},
	}
	for _, args := range cmds {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}

	result, err := CheckWriteSetConflict(dir, base, []string{"dispatch.go"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.HasConflict {
		t.Error("expected no conflict when files don't overlap")
	}
	if len(result.ConcurrentFiles) != 1 || result.ConcurrentFiles[0] != "other.go" {
		t.Errorf("ConcurrentFiles = %v, want [other.go]", result.ConcurrentFiles)
	}
}

func TestCheckWriteSetConflict_WriteWriteConflict(t *testing.T) {
	dir := initGitRepo(t)
	base, _ := gitHeadCommit(dir)

	// Make a concurrent change to the same file
	if err := os.WriteFile(filepath.Join(dir, "shared.go"), []byte("package shared\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cmds := [][]string{
		{"git", "-C", dir, "add", "shared.go"},
		{"git", "-C", dir, "commit", "-m", "add shared"},
	}
	for _, args := range cmds {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}

	result, err := CheckWriteSetConflict(dir, base, []string{"shared.go", "other.go"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.HasConflict {
		t.Error("expected write-write conflict")
	}
	if len(result.ConflictFiles) != 1 || result.ConflictFiles[0] != "shared.go" {
		t.Errorf("ConflictFiles = %v, want [shared.go]", result.ConflictFiles)
	}
}

func TestListConcurrentDispatches(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	scope := "test-run-1"

	// Create two dispatches with base commits
	d1 := &Dispatch{
		AgentType:      "codex",
		ProjectDir:     "/tmp/proj",
		ScopeID:        &scope,
		BaseRepoCommit: strPtr("abc123"),
	}
	d2 := &Dispatch{
		AgentType:      "codex",
		ProjectDir:     "/tmp/proj",
		ScopeID:        &scope,
		BaseRepoCommit: strPtr("abc123"),
	}
	d3 := &Dispatch{
		AgentType:  "codex",
		ProjectDir: "/tmp/proj",
		ScopeID:    &scope,
		// no base commit — should be excluded
	}

	id1, _ := store.Create(ctx, d1)
	id2, _ := store.Create(ctx, d2)
	store.Create(ctx, d3) // id3 excluded

	concurrent, err := store.ListConcurrentDispatches(ctx, id1, scope)
	if err != nil {
		t.Fatalf("ListConcurrentDispatches: %v", err)
	}

	// Should return d2 (not d1 or d3)
	if len(concurrent) != 1 {
		t.Fatalf("got %d concurrent dispatches, want 1", len(concurrent))
	}
	if concurrent[0].ID != id2 {
		t.Errorf("got dispatch %s, want %s", concurrent[0].ID, id2)
	}
}

func TestGitDiffFiles(t *testing.T) {
	dir := initGitRepo(t)
	base, _ := gitHeadCommit(dir)

	// Create multiple files and commit
	for _, name := range []string{"a.go", "b.go", "c.go"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("package x\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	cmds := [][]string{
		{"git", "-C", dir, "add", "."},
		{"git", "-C", dir, "commit", "-m", "add files"},
	}
	for _, args := range cmds {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}

	head, _ := gitHeadCommit(dir)
	files, err := gitDiffFiles(dir, base, head)
	if err != nil {
		t.Fatalf("gitDiffFiles: %v", err)
	}
	if len(files) != 3 {
		t.Errorf("got %d files, want 3: %v", len(files), files)
	}
}

func TestExtractWriteSet_NoDivergence(t *testing.T) {
	dir := initGitRepo(t)
	head, _ := gitHeadCommit(dir)

	files, err := ExtractWriteSet(dir, head)
	if err != nil {
		t.Fatalf("ExtractWriteSet: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("expected empty write-set, got %v", files)
	}
}
