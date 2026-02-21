package dispatch

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// MergeIntent represents a pending or completed merge operation.
// The intent record ensures atomicity between SQLite and git operations
// using the Transactional Outbox pattern.
type MergeIntent struct {
	ID            int64
	DispatchID    string
	RunID         *string
	BaseCommit    string
	PatchHash     *string
	Status        string // "pending", "completed", "failed"
	ResultCommit  *string
	ConflictFiles *string
	ErrorMessage  *string
	CreatedAt     int64
	CompletedAt   *int64
}

const (
	IntentStatusPending   = "pending"
	IntentStatusCompleted = "completed"
	IntentStatusFailed    = "failed"
)

// IntentStore provides merge intent operations.
type IntentStore struct {
	db *sql.DB
}

// NewIntentStore creates an intent store.
func NewIntentStore(db *sql.DB) *IntentStore {
	return &IntentStore{db: db}
}

// RecordIntent creates a pending merge intent. This is Phase 1 of the
// outbox pattern — a short transaction (~1ms) that records the intent
// before any git work begins.
func (s *IntentStore) RecordIntent(ctx context.Context, dispatchID, runID, baseCommit, patchHash string) (int64, error) {
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO merge_intents (dispatch_id, run_id, base_commit, patch_hash, status)
		VALUES (?, NULLIF(?, ''), ?, NULLIF(?, ''), ?)`,
		dispatchID, runID, baseCommit, patchHash, IntentStatusPending,
	)
	if err != nil {
		return 0, fmt.Errorf("record intent: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("record intent: last id: %w", err)
	}
	return id, nil
}

// CompleteIntent marks an intent as completed with the result commit SHA.
// This is Phase 3 of the outbox pattern — records the outcome after
// git work succeeds.
func (s *IntentStore) CompleteIntent(ctx context.Context, intentID int64, resultCommit string) error {
	now := time.Now().Unix()
	result, err := s.db.ExecContext(ctx, `
		UPDATE merge_intents
		SET status = ?, result_commit = ?, completed_at = ?
		WHERE id = ? AND status = ?`,
		IntentStatusCompleted, resultCommit, now,
		intentID, IntentStatusPending,
	)
	if err != nil {
		return fmt.Errorf("complete intent: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("complete intent: no pending intent with id %d", intentID)
	}
	return nil
}

// FailIntent marks an intent as failed with an error message and
// optional conflict file list.
func (s *IntentStore) FailIntent(ctx context.Context, intentID int64, errMsg string, conflictFiles []string) error {
	now := time.Now().Unix()
	var conflicts *string
	if len(conflictFiles) > 0 {
		joined := strings.Join(conflictFiles, "\n")
		conflicts = &joined
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE merge_intents
		SET status = ?, error_message = ?, conflict_files = ?, completed_at = ?
		WHERE id = ? AND status = ?`,
		IntentStatusFailed, errMsg, conflicts, now,
		intentID, IntentStatusPending,
	)
	if err != nil {
		return fmt.Errorf("fail intent: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("fail intent: no pending intent with id %d", intentID)
	}
	return nil
}

// ListPendingIntents returns all intents in "pending" status.
// Used for crash recovery: on startup, scan these and resolve each one
// by checking whether the git commit actually landed.
func (s *IntentStore) ListPendingIntents(ctx context.Context) ([]*MergeIntent, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, dispatch_id, run_id, base_commit, patch_hash,
			status, result_commit, conflict_files, error_message,
			created_at, completed_at
		FROM merge_intents
		WHERE status = ?
		ORDER BY created_at ASC`,
		IntentStatusPending,
	)
	if err != nil {
		return nil, fmt.Errorf("list pending intents: %w", err)
	}
	defer rows.Close()
	return scanIntents(rows)
}

// GetByDispatch returns the most recent intent for a dispatch.
func (s *IntentStore) GetByDispatch(ctx context.Context, dispatchID string) (*MergeIntent, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, dispatch_id, run_id, base_commit, patch_hash,
			status, result_commit, conflict_files, error_message,
			created_at, completed_at
		FROM merge_intents
		WHERE dispatch_id = ?
		ORDER BY created_at DESC
		LIMIT 1`,
		dispatchID,
	)
	return scanIntent(row)
}

// RecoverPendingIntents resolves all pending intents by checking git state.
// For each pending intent, it checks whether a commit exists at HEAD that
// is different from the base commit. If so, the intent is completed with
// the current HEAD. If not (or on error), the intent is failed.
func (s *IntentStore) RecoverPendingIntents(ctx context.Context, projectDir string) (recovered, failed int, err error) {
	intents, err := s.ListPendingIntents(ctx)
	if err != nil {
		return 0, 0, err
	}

	for _, intent := range intents {
		head, gitErr := gitHeadCommit(projectDir)
		if gitErr != nil {
			if failErr := s.FailIntent(ctx, intent.ID, fmt.Sprintf("recovery: git error: %v", gitErr), nil); failErr != nil {
				return recovered, failed, fmt.Errorf("recover: fail intent %d: %w", intent.ID, failErr)
			}
			failed++
			continue
		}

		if head != intent.BaseCommit {
			// Git has advanced past the base — the merge likely succeeded
			if err := s.CompleteIntent(ctx, intent.ID, head); err != nil {
				return recovered, failed, fmt.Errorf("recover: complete intent %d: %w", intent.ID, err)
			}
			recovered++
		} else {
			// HEAD is still at base — the merge didn't happen
			if err := s.FailIntent(ctx, intent.ID, "recovery: HEAD unchanged from base commit", nil); err != nil {
				return recovered, failed, fmt.Errorf("recover: fail intent %d: %w", intent.ID, err)
			}
			failed++
		}
	}

	return recovered, failed, nil
}

// gitCommitExists checks if a commit SHA exists in the repository.
func gitCommitExists(dir, sha string) bool {
	cmd := exec.Command("git", "-C", dir, "cat-file", "-t", sha)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return false
	}
	return strings.TrimSpace(out.String()) == "commit"
}

func scanIntents(rows *sql.Rows) ([]*MergeIntent, error) {
	var intents []*MergeIntent
	for rows.Next() {
		intent, err := scanIntentFromRow(rows)
		if err != nil {
			return nil, err
		}
		intents = append(intents, intent)
	}
	return intents, rows.Err()
}

func scanIntent(row *sql.Row) (*MergeIntent, error) {
	var (
		runID         sql.NullString
		patchHash     sql.NullString
		resultCommit  sql.NullString
		conflictFiles sql.NullString
		errorMessage  sql.NullString
		completedAt   sql.NullInt64
	)
	intent := &MergeIntent{}
	err := row.Scan(
		&intent.ID, &intent.DispatchID, &runID, &intent.BaseCommit,
		&patchHash, &intent.Status, &resultCommit, &conflictFiles,
		&errorMessage, &intent.CreatedAt, &completedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("scan intent: %w", err)
	}
	intent.RunID = nullStr(runID)
	intent.PatchHash = nullStr(patchHash)
	intent.ResultCommit = nullStr(resultCommit)
	intent.ConflictFiles = nullStr(conflictFiles)
	intent.ErrorMessage = nullStr(errorMessage)
	intent.CompletedAt = nullInt64(completedAt)
	return intent, nil
}

type intentRowScanner interface {
	Scan(dest ...interface{}) error
}

func scanIntentFromRow(scanner intentRowScanner) (*MergeIntent, error) {
	var (
		runID         sql.NullString
		patchHash     sql.NullString
		resultCommit  sql.NullString
		conflictFiles sql.NullString
		errorMessage  sql.NullString
		completedAt   sql.NullInt64
	)
	intent := &MergeIntent{}
	err := scanner.Scan(
		&intent.ID, &intent.DispatchID, &runID, &intent.BaseCommit,
		&patchHash, &intent.Status, &resultCommit, &conflictFiles,
		&errorMessage, &intent.CreatedAt, &completedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("scan intent: %w", err)
	}
	intent.RunID = nullStr(runID)
	intent.PatchHash = nullStr(patchHash)
	intent.ResultCommit = nullStr(resultCommit)
	intent.ConflictFiles = nullStr(conflictFiles)
	intent.ErrorMessage = nullStr(errorMessage)
	intent.CompletedAt = nullInt64(completedAt)
	return intent, nil
}
