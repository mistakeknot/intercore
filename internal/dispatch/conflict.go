package dispatch

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// ConflictResult holds the outcome of a write-set conflict check.
type ConflictResult struct {
	HasConflict    bool     // true if write-sets overlap
	ConflictFiles  []string // files modified by both the dispatch and concurrent changes
	BaseCommit     string   // the dispatch's starting commit
	CurrentCommit  string   // HEAD at check time
	ConcurrentFiles []string // files changed between base and current HEAD
	DispatchFiles   []string // files the dispatch modified (from its output)
}

// ConflictNone is a sentinel for "no conflict detected".
const (
	ConflictTypeWriteWrite = "write_write"
	ConflictTypeNone       = ""
)

// CheckWriteSetConflict detects write-write conflicts between a dispatch's
// changes and concurrent changes since the dispatch was spawned.
//
// Algorithm (Snapshot Isolation / Alternate A):
//  1. Compare dispatch's base_repo_commit against current HEAD
//  2. If same → no divergence, no conflict possible
//  3. If diverged → extract files changed between base..HEAD (concurrent changes)
//  4. Extract files the dispatch modified (from its patch/diff output)
//  5. Intersect the two sets → any overlap is a write-write conflict
//
// projectDir must be the git repository directory.
// dispatchFiles is the set of files the dispatch modified (caller provides this).
func CheckWriteSetConflict(projectDir, baseCommit string, dispatchFiles []string) (*ConflictResult, error) {
	if baseCommit == "" {
		return &ConflictResult{}, nil // no base commit recorded, skip check
	}

	// Get current HEAD
	currentCommit, err := gitHeadCommit(projectDir)
	if err != nil {
		return nil, fmt.Errorf("conflict check: get HEAD: %w", err)
	}

	result := &ConflictResult{
		BaseCommit:    baseCommit,
		CurrentCommit: currentCommit,
		DispatchFiles: dispatchFiles,
	}

	// No divergence — no conflict possible
	if currentCommit == baseCommit {
		return result, nil
	}

	// Get files changed between base and current HEAD
	concurrentFiles, err := gitDiffFiles(projectDir, baseCommit, currentCommit)
	if err != nil {
		return nil, fmt.Errorf("conflict check: diff base..HEAD: %w", err)
	}
	result.ConcurrentFiles = concurrentFiles

	// Build set from concurrent changes for O(n) intersection
	concurrentSet := make(map[string]bool, len(concurrentFiles))
	for _, f := range concurrentFiles {
		concurrentSet[f] = true
	}

	// Find overlapping files
	for _, f := range dispatchFiles {
		if concurrentSet[f] {
			result.ConflictFiles = append(result.ConflictFiles, f)
		}
	}

	result.HasConflict = len(result.ConflictFiles) > 0
	return result, nil
}

// ExtractWriteSet extracts the list of files modified by a dispatch.
// It examines the dispatch's output directory for a .patch or .diff file,
// or falls back to git diff against the base commit.
func ExtractWriteSet(projectDir, baseCommit string) ([]string, error) {
	if baseCommit == "" {
		return nil, nil
	}

	// Use git diff to find files changed since base commit
	currentCommit, err := gitHeadCommit(projectDir)
	if err != nil {
		return nil, fmt.Errorf("extract write-set: get HEAD: %w", err)
	}
	if currentCommit == baseCommit {
		return nil, nil // no changes
	}

	return gitDiffFiles(projectDir, baseCommit, currentCommit)
}

// gitDiffFiles returns the list of files changed between two commits.
func gitDiffFiles(dir, from, to string) ([]string, error) {
	cmd := exec.Command("git", "-C", dir, "diff", "--name-only", from, to)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return nil, err
	}

	raw := strings.TrimSpace(out.String())
	if raw == "" {
		return nil, nil
	}
	return strings.Split(raw, "\n"), nil
}

// ListConcurrentDispatches returns non-terminal dispatches in the same scope
// that overlap in time with the given dispatch (spawned before it completed).
func (s *Store) ListConcurrentDispatches(ctx context.Context, dispatchID, scopeID string) ([]*Dispatch, error) {
	if scopeID == "" {
		return nil, nil
	}

	return s.queryDispatches(ctx, `
		SELECT `+dispatchCols+` FROM dispatches
		WHERE scope_id = ?
		  AND id != ?
		  AND base_repo_commit IS NOT NULL
		  AND status IN ('spawned', 'running', 'completed')
		ORDER BY created_at ASC`,
		scopeID, dispatchID,
	)
}
