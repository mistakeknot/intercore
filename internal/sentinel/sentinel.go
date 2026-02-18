package sentinel

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"
)

// Sentinel represents a throttle guard row.
type Sentinel struct {
	Name      string
	ScopeID   string
	LastFired int64
}

// Store provides sentinel operations against the intercore DB.
type Store struct {
	db *sql.DB
}

// New creates a sentinel store.
func New(db *sql.DB) *Store {
	return &Store{db: db}
}

// Check performs an atomic claim-if-eligible throttle check.
// Returns true if allowed (sentinel fired), false if throttled.
// When intervalSec is 0, the sentinel fires at most once per scope_id.
func (s *Store) Check(ctx context.Context, name, scopeID string, intervalSec int) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Ensure row exists (INSERT OR IGNORE for first-time sentinels)
	_, err = tx.ExecContext(ctx,
		"INSERT OR IGNORE INTO sentinels (name, scope_id, last_fired) VALUES (?, ?, 0)",
		name, scopeID)
	if err != nil {
		return false, fmt.Errorf("ensure sentinel: %w", err)
	}

	// Atomic conditional UPDATE with RETURNING
	// CTE wrapping UPDATE RETURNING is not supported by modernc.org/sqlite,
	// so we use direct UPDATE ... RETURNING and count rows.
	// Outer parentheses required for correct precedence in WHERE clause.
	rows, err := tx.QueryContext(ctx, `
		UPDATE sentinels
		SET last_fired = unixepoch()
		WHERE name = ? AND scope_id = ?
		  AND ((? = 0 AND last_fired = 0)
		       OR (? > 0 AND unixepoch() - last_fired >= ?))
		RETURNING 1`,
		name, scopeID, intervalSec, intervalSec, intervalSec,
	)
	if err != nil {
		return false, fmt.Errorf("sentinel check: %w", err)
	}
	allowed := 0
	for rows.Next() {
		allowed++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("sentinel check rows: %w", err)
	}

	// Synchronous auto-prune: delete stale sentinels in same tx
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM sentinels WHERE unixepoch() - last_fired > 604800"); err != nil {
		fmt.Fprintf(os.Stderr, "ic: auto-prune: %v\n", err)
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	return allowed == 1, nil
}

// Reset clears a sentinel, allowing it to fire again.
func (s *Store) Reset(ctx context.Context, name, scopeID string) error {
	_, err := s.db.ExecContext(ctx,
		"DELETE FROM sentinels WHERE name = ? AND scope_id = ?",
		name, scopeID)
	if err != nil {
		return fmt.Errorf("reset: %w", err)
	}
	return nil
}

// List returns all active sentinels.
func (s *Store) List(ctx context.Context) ([]Sentinel, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT name, scope_id, last_fired FROM sentinels ORDER BY name, scope_id")
	if err != nil {
		return nil, fmt.Errorf("list: %w", err)
	}
	defer rows.Close()

	var sentinels []Sentinel
	for rows.Next() {
		var sent Sentinel
		if err := rows.Scan(&sent.Name, &sent.ScopeID, &sent.LastFired); err != nil {
			return nil, fmt.Errorf("list scan: %w", err)
		}
		sentinels = append(sentinels, sent)
	}
	return sentinels, rows.Err()
}

// Prune deletes sentinels older than the given duration.
func (s *Store) Prune(ctx context.Context, olderThan time.Duration) (int64, error) {
	threshold := time.Now().Unix() - int64(olderThan.Seconds())
	result, err := s.db.ExecContext(ctx,
		"DELETE FROM sentinels WHERE last_fired < ?",
		threshold)
	if err != nil {
		return 0, fmt.Errorf("prune: %w", err)
	}
	return result.RowsAffected()
}
