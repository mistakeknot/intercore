package coordination

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"math/big"
	"time"
)

const idChars = "abcdefghijklmnopqrstuvwxyz0123456789"
const idLen = 8

func generateID() (string, error) {
	b := make([]byte, idLen)
	max := big.NewInt(int64(len(idChars)))
	for i := range b {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", fmt.Errorf("generate id: %w", err)
		}
		b[i] = idChars[n.Int64()]
	}
	return string(b), nil
}

// EventFunc is called after coordination state changes.
// eventType is one of: coordination.acquired, .released, .conflict, .expired, .transferred.
type EventFunc func(ctx context.Context, eventType, lockID, owner, pattern, scope, reason, runID string) error

// Store provides coordination lock operations backed by SQLite.
type Store struct {
	db      *sql.DB
	onEvent EventFunc
}

// NewStore creates a coordination store.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// SetEventFunc sets the event callback. Call after NewStore, before Reserve/Release.
func (s *Store) SetEventFunc(fn EventFunc) {
	s.onEvent = fn
}

func (s *Store) emitEvent(ctx context.Context, eventType, lockID, owner, pattern, scope, reason, runID string) {
	if s.onEvent != nil {
		_ = s.onEvent(ctx, eventType, lockID, owner, pattern, scope, reason, runID)
	}
}

// Reserve acquires a coordination lock. Uses BEGIN IMMEDIATE for serializable writes.
func (s *Store) Reserve(ctx context.Context, lock Lock) (*ReserveResult, error) {
	// Validate glob complexity BEFORE any DB access to prevent DoS.
	if err := ValidateComplexity(lock.Pattern); err != nil {
		return nil, fmt.Errorf("invalid pattern: %w", err)
	}

	if lock.ID == "" {
		id, err := generateID()
		if err != nil {
			return nil, err
		}
		lock.ID = id
	}
	lock.CreatedAt = time.Now().Unix()
	if lock.TTLSeconds > 0 {
		exp := lock.CreatedAt + int64(lock.TTLSeconds)
		lock.ExpiresAt = &exp
	}

	// BEGIN IMMEDIATE via LevelSerializable — modernc.org/sqlite maps this correctly.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, fmt.Errorf("begin immediate: %w", err)
	}
	defer tx.Rollback()

	// Inline sweep of expired locks (same-transaction, sentinel pattern).
	// NOTE: Inline sweep does NOT emit events (performance tradeoff).
	now := time.Now().Unix()
	_, _ = tx.ExecContext(ctx, `UPDATE coordination_locks SET released_at = ?
		WHERE released_at IS NULL AND expires_at IS NOT NULL AND expires_at < ?`, now, now)

	// Check for conflicts
	rows, err := tx.QueryContext(ctx, `SELECT id, owner, pattern, reason, exclusive
		FROM coordination_locks
		WHERE scope = ? AND released_at IS NULL
		  AND (expires_at IS NULL OR expires_at > ?)
		  AND owner != ?`, lock.Scope, now, lock.Owner)
	if err != nil {
		return nil, fmt.Errorf("query conflicts: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var existing struct {
			id, owner, pattern string
			reason             sql.NullString
			exclusive          bool
		}
		if err := rows.Scan(&existing.id, &existing.owner, &existing.pattern, &existing.reason, &existing.exclusive); err != nil {
			return nil, err
		}
		// Skip shared+shared
		if !lock.Exclusive && !existing.exclusive {
			continue
		}
		overlap, err := PatternsOverlap(lock.Pattern, existing.pattern)
		if err != nil {
			return nil, fmt.Errorf("overlap check: %w", err)
		}
		if overlap {
			conflict := &ConflictInfo{
				BlockerID:      existing.id,
				BlockerOwner:   existing.owner,
				BlockerPattern: existing.pattern,
				BlockerReason:  existing.reason.String,
			}
			s.emitEvent(ctx, "coordination.conflict", lock.ID, lock.Owner, lock.Pattern, lock.Scope, existing.owner, lock.RunID)
			return &ReserveResult{Conflict: conflict}, nil
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Insert the lock
	_, err = tx.ExecContext(ctx, `INSERT INTO coordination_locks
		(id, type, owner, scope, pattern, exclusive, reason, ttl_seconds, created_at, expires_at, dispatch_id, run_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		lock.ID, lock.Type, lock.Owner, lock.Scope, lock.Pattern, lock.Exclusive,
		nullStr(lock.Reason), nullInt(lock.TTLSeconds), lock.CreatedAt, lock.ExpiresAt,
		nullStr(lock.DispatchID), nullStr(lock.RunID))
	if err != nil {
		return nil, fmt.Errorf("insert: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	s.emitEvent(ctx, "coordination.acquired", lock.ID, lock.Owner, lock.Pattern, lock.Scope, lock.Reason, lock.RunID)
	return &ReserveResult{Lock: &lock}, nil
}

// Release marks a lock as released. Releases by ID or by owner+scope.
func (s *Store) Release(ctx context.Context, id, owner, scope string) (int64, error) {
	now := time.Now().Unix()
	var res sql.Result
	var err error

	if id != "" {
		res, err = s.db.ExecContext(ctx,
			`UPDATE coordination_locks SET released_at = ? WHERE id = ? AND released_at IS NULL`, now, id)
	} else if owner != "" && scope != "" {
		res, err = s.db.ExecContext(ctx,
			`UPDATE coordination_locks SET released_at = ? WHERE owner = ? AND scope = ? AND released_at IS NULL`,
			now, owner, scope)
	} else {
		return 0, fmt.Errorf("release requires id or owner+scope")
	}
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		s.emitEvent(ctx, "coordination.released", id, owner, "", scope, "", "")
	}
	return n, nil
}

// Check returns conflicting active locks for a given pattern in a scope.
func (s *Store) Check(ctx context.Context, scope, pattern, excludeOwner string) ([]Lock, error) {
	// Validate glob complexity BEFORE any DB access to prevent DoS.
	if err := ValidateComplexity(pattern); err != nil {
		return nil, fmt.Errorf("invalid pattern: %w", err)
	}

	now := time.Now().Unix()
	query := `SELECT id, type, owner, scope, pattern, exclusive, reason, ttl_seconds,
		created_at, expires_at, released_at, dispatch_id, run_id
		FROM coordination_locks
		WHERE scope = ? AND released_at IS NULL AND (expires_at IS NULL OR expires_at > ?)`
	args := []any{scope, now}
	if excludeOwner != "" {
		query += " AND owner != ?"
		args = append(args, excludeOwner)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var conflicts []Lock
	for rows.Next() {
		var l Lock
		if err := scanLock(rows, &l); err != nil {
			return nil, err
		}
		overlap, err := PatternsOverlap(pattern, l.Pattern)
		if err != nil {
			return nil, err
		}
		if overlap {
			conflicts = append(conflicts, l)
		}
	}
	return conflicts, rows.Err()
}

// List returns locks matching the filter.
func (s *Store) List(ctx context.Context, f ListFilter) ([]Lock, error) {
	now := time.Now().Unix()
	query := `SELECT id, type, owner, scope, pattern, exclusive, reason, ttl_seconds,
		created_at, expires_at, released_at, dispatch_id, run_id
		FROM coordination_locks WHERE 1=1`
	var args []any

	if f.Scope != "" {
		query += " AND scope = ?"
		args = append(args, f.Scope)
	}
	if f.Owner != "" {
		query += " AND owner = ?"
		args = append(args, f.Owner)
	}
	if f.Type != "" {
		query += " AND type = ?"
		args = append(args, f.Type)
	}
	if f.Active {
		query += " AND released_at IS NULL AND (expires_at IS NULL OR expires_at > ?)"
		args = append(args, now)
	}
	query += " ORDER BY created_at DESC"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var locks []Lock
	for rows.Next() {
		var l Lock
		if err := scanLock(rows, &l); err != nil {
			return nil, err
		}
		locks = append(locks, l)
	}
	return locks, rows.Err()
}

// Transfer atomically reassigns all active locks from one owner to another.
func (s *Store) Transfer(ctx context.Context, fromOwner, toOwner, scope string, force bool) (int64, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return 0, fmt.Errorf("begin immediate: %w", err)
	}
	defer tx.Rollback()

	now := time.Now().Unix()

	if !force {
		// Check for conflicts: would the transferred locks conflict with toOwner's existing locks?
		fromRows, err := tx.QueryContext(ctx, `SELECT pattern FROM coordination_locks
			WHERE owner = ? AND scope = ? AND released_at IS NULL
			AND (expires_at IS NULL OR expires_at > ?) AND exclusive = 1`, fromOwner, scope, now)
		if err != nil {
			return 0, err
		}
		var fromPatterns []string
		for fromRows.Next() {
			var p string
			if err := fromRows.Scan(&p); err != nil {
				fromRows.Close()
				return 0, fmt.Errorf("scan from-patterns: %w", err)
			}
			fromPatterns = append(fromPatterns, p)
		}
		fromRows.Close()
		if err := fromRows.Err(); err != nil {
			return 0, fmt.Errorf("read from-patterns: %w", err)
		}

		toRows, err := tx.QueryContext(ctx, `SELECT pattern FROM coordination_locks
			WHERE owner = ? AND scope = ? AND released_at IS NULL
			AND (expires_at IS NULL OR expires_at > ?) AND exclusive = 1`, toOwner, scope, now)
		if err != nil {
			return 0, err
		}
		for toRows.Next() {
			var toPattern string
			if err := toRows.Scan(&toPattern); err != nil {
				toRows.Close()
				return 0, fmt.Errorf("scan to-patterns: %w", err)
			}
			for _, fp := range fromPatterns {
				overlap, err := PatternsOverlap(fp, toPattern)
				if err != nil {
					toRows.Close()
					return 0, fmt.Errorf("overlap check in transfer: %w", err)
				}
				if overlap {
					toRows.Close()
					return 0, fmt.Errorf("transfer conflict: %s overlaps with existing lock %s", fp, toPattern)
				}
			}
		}
		toRows.Close()
		if err := toRows.Err(); err != nil {
			return 0, fmt.Errorf("read to-patterns: %w", err)
		}
	}

	// Perform the transfer
	res, err := tx.ExecContext(ctx, `UPDATE coordination_locks SET owner = ?
		WHERE owner = ? AND scope = ? AND released_at IS NULL
		AND (expires_at IS NULL OR expires_at > ?)`, toOwner, fromOwner, scope, now)
	if err != nil {
		return 0, err
	}

	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, err
	}

	if n > 0 {
		s.emitEvent(ctx, "coordination.transferred", "", fromOwner, "", scope, "transferred to "+toOwner, "")
	}
	return n, nil
}

// Sweep cleans expired locks by TTL.
func (s *Store) Sweep(ctx context.Context, olderThan time.Duration, dryRun bool) (*SweepResult, error) {
	now := time.Now().Unix()
	cutoff := now
	if olderThan > 0 {
		cutoff = now - int64(olderThan.Seconds())
	}
	result := &SweepResult{}

	rows, err := s.db.QueryContext(ctx, `SELECT id, type, owner, scope, pattern, exclusive,
		reason, ttl_seconds, created_at, expires_at, released_at, dispatch_id, run_id
		FROM coordination_locks
		WHERE released_at IS NULL AND expires_at IS NOT NULL AND expires_at < ?`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var expired []Lock
	for rows.Next() {
		var l Lock
		if err := scanLock(rows, &l); err != nil {
			return nil, err
		}
		expired = append(expired, l)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	result.Expired = len(expired)
	result.Total = result.Expired

	if dryRun || result.Total == 0 {
		return result, nil
	}

	for _, l := range expired {
		if _, err := s.Release(ctx, l.ID, "", ""); err != nil {
			continue
		}
		s.emitEvent(ctx, "coordination.expired", l.ID, l.Owner, l.Pattern, l.Scope, "sweep", l.RunID)
	}
	return result, nil
}

func scanLock(rows *sql.Rows, l *Lock) error {
	var expiresAt, releasedAt sql.NullInt64
	var reason, dispatchID, runID sql.NullString
	var ttlSeconds sql.NullInt64
	err := rows.Scan(&l.ID, &l.Type, &l.Owner, &l.Scope, &l.Pattern, &l.Exclusive,
		&reason, &ttlSeconds, &l.CreatedAt, &expiresAt, &releasedAt,
		&dispatchID, &runID)
	if err != nil {
		return err
	}
	l.Reason = reason.String
	if ttlSeconds.Valid {
		l.TTLSeconds = int(ttlSeconds.Int64)
	}
	if expiresAt.Valid {
		l.ExpiresAt = &expiresAt.Int64
	}
	if releasedAt.Valid {
		l.ReleasedAt = &releasedAt.Int64
	}
	l.DispatchID = dispatchID.String
	l.RunID = runID.String
	return nil
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt(i int) any {
	if i == 0 {
		return nil
	}
	return i
}
