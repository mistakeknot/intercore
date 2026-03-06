package landed

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Change represents a commit that reached the trunk branch.
type Change struct {
	ID            int64   `json:"id"`
	CommitSHA     string  `json:"commit_sha"`
	ProjectDir    string  `json:"project_dir"`
	Branch        string  `json:"branch"`
	DispatchID    *string `json:"dispatch_id,omitempty"`
	RunID         *string `json:"run_id,omitempty"`
	BeadID        *string `json:"bead_id,omitempty"`
	SessionID     *string `json:"session_id,omitempty"`
	MergeIntentID *int64  `json:"merge_intent_id,omitempty"`
	LandedAt      int64   `json:"landed_at"`
	RevertedAt    *int64  `json:"reverted_at,omitempty"`
	RevertedBy    *string `json:"reverted_by,omitempty"`
	FilesChanged  *int    `json:"files_changed,omitempty"`
	Insertions    *int    `json:"insertions,omitempty"`
	Deletions     *int    `json:"deletions,omitempty"`
}

// RecordOpts holds the fields for recording a landed change.
type RecordOpts struct {
	CommitSHA     string
	ProjectDir    string
	Branch        string // defaults to "main" if empty
	DispatchID    string
	RunID         string
	BeadID        string
	SessionID     string
	MergeIntentID int64
	FilesChanged  int
	Insertions    int
	Deletions     int
}

// ListOpts filters landed changes queries.
type ListOpts struct {
	ProjectDir      string
	BeadID          string
	RunID           string
	SessionID       string
	Since           int64 // unix epoch
	Until           int64
	IncludeReverted bool
	Limit           int
}

// Summary holds aggregated landed-change statistics.
type Summary struct {
	Total        int            `json:"total"`
	Reverted     int            `json:"reverted"`
	ByBead       map[string]int `json:"by_bead,omitempty"`
	ByRun        map[string]int `json:"by_run,omitempty"`
	FirstLanding int64          `json:"first_landing,omitempty"`
	LastLanding  int64          `json:"last_landing,omitempty"`
}

// Store provides landed-change operations.
type Store struct {
	db *sql.DB
}

// NewStore creates a landed-change store.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// Record inserts a landed change. Returns the row ID.
// Idempotent: if the (commit_sha, project_dir) pair already exists,
// the existing row is returned without error.
func (s *Store) Record(ctx context.Context, opts RecordOpts) (int64, error) {
	branch := opts.Branch
	if branch == "" {
		branch = "main"
	}

	result, err := s.db.ExecContext(ctx, `
		INSERT INTO landed_changes (
			commit_sha, project_dir, branch,
			dispatch_id, run_id, bead_id, session_id,
			merge_intent_id,
			files_changed, insertions, deletions
		) VALUES (?, ?, ?,
			NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''),
			NULLIF(?, 0),
			NULLIF(?, 0), NULLIF(?, 0), NULLIF(?, 0))
		ON CONFLICT(commit_sha, project_dir) DO NOTHING`,
		opts.CommitSHA, opts.ProjectDir, branch,
		opts.DispatchID, opts.RunID, opts.BeadID, opts.SessionID,
		opts.MergeIntentID,
		opts.FilesChanged, opts.Insertions, opts.Deletions,
	)
	if err != nil {
		return 0, fmt.Errorf("record landed change: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("record landed change: last id: %w", err)
	}

	// ON CONFLICT DO NOTHING returns id=0 — look up the existing row
	if id == 0 {
		row := s.db.QueryRowContext(ctx,
			`SELECT id FROM landed_changes WHERE commit_sha = ? AND project_dir = ?`,
			opts.CommitSHA, opts.ProjectDir,
		)
		if err := row.Scan(&id); err != nil {
			return 0, fmt.Errorf("record landed change: lookup existing: %w", err)
		}
	}

	return id, nil
}

// MarkReverted sets the reverted_at and reverted_by fields on a landed change.
func (s *Store) MarkReverted(ctx context.Context, commitSHA, projectDir, revertedBy string) error {
	now := time.Now().Unix()
	result, err := s.db.ExecContext(ctx, `
		UPDATE landed_changes
		SET reverted_at = ?, reverted_by = ?
		WHERE commit_sha = ? AND project_dir = ? AND reverted_at IS NULL`,
		now, revertedBy,
		commitSHA, projectDir,
	)
	if err != nil {
		return fmt.Errorf("mark reverted: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("mark reverted: no un-reverted landed change for %s in %s", commitSHA, projectDir)
	}
	return nil
}

// List returns landed changes matching the given filters.
func (s *Store) List(ctx context.Context, opts ListOpts) ([]Change, error) {
	var where []string
	var args []interface{}

	if opts.ProjectDir != "" {
		where = append(where, "project_dir = ?")
		args = append(args, opts.ProjectDir)
	}
	if opts.BeadID != "" {
		where = append(where, "bead_id = ?")
		args = append(args, opts.BeadID)
	}
	if opts.RunID != "" {
		where = append(where, "run_id = ?")
		args = append(args, opts.RunID)
	}
	if opts.SessionID != "" {
		where = append(where, "session_id = ?")
		args = append(args, opts.SessionID)
	}
	if opts.Since > 0 {
		where = append(where, "landed_at >= ?")
		args = append(args, opts.Since)
	}
	if opts.Until > 0 {
		where = append(where, "landed_at <= ?")
		args = append(args, opts.Until)
	}
	if !opts.IncludeReverted {
		where = append(where, "reverted_at IS NULL")
	}

	query := "SELECT id, commit_sha, project_dir, branch, dispatch_id, run_id, bead_id, session_id, merge_intent_id, landed_at, reverted_at, reverted_by, files_changed, insertions, deletions FROM landed_changes"
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY landed_at DESC"

	limit := opts.Limit
	if limit <= 0 {
		limit = 1000
	}
	query += fmt.Sprintf(" LIMIT %d", limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list landed changes: %w", err)
	}
	defer rows.Close()

	var changes []Change
	for rows.Next() {
		var c Change
		var dispatchID, runID, beadID, sessionID, revertedBy sql.NullString
		var mergeIntentID sql.NullInt64
		var revertedAt sql.NullInt64
		var filesChanged, insertions, deletions sql.NullInt32

		if err := rows.Scan(
			&c.ID, &c.CommitSHA, &c.ProjectDir, &c.Branch,
			&dispatchID, &runID, &beadID, &sessionID,
			&mergeIntentID, &c.LandedAt,
			&revertedAt, &revertedBy,
			&filesChanged, &insertions, &deletions,
		); err != nil {
			return nil, fmt.Errorf("list landed changes: scan: %w", err)
		}

		c.DispatchID = nullStr(dispatchID)
		c.RunID = nullStr(runID)
		c.BeadID = nullStr(beadID)
		c.SessionID = nullStr(sessionID)
		c.RevertedBy = nullStr(revertedBy)
		c.MergeIntentID = nullInt64(mergeIntentID)
		c.RevertedAt = nullInt64Ptr(revertedAt)
		c.FilesChanged = nullInt32Ptr(filesChanged)
		c.Insertions = nullInt32Ptr(insertions)
		c.Deletions = nullInt32Ptr(deletions)

		changes = append(changes, c)
	}
	return changes, rows.Err()
}

// Summary returns aggregated statistics for landed changes.
func (s *Store) Summary(ctx context.Context, opts ListOpts) (*Summary, error) {
	var where []string
	var args []interface{}

	if opts.ProjectDir != "" {
		where = append(where, "project_dir = ?")
		args = append(args, opts.ProjectDir)
	}
	if opts.BeadID != "" {
		where = append(where, "bead_id = ?")
		args = append(args, opts.BeadID)
	}
	if opts.Since > 0 {
		where = append(where, "landed_at >= ?")
		args = append(args, opts.Since)
	}
	if opts.Until > 0 {
		where = append(where, "landed_at <= ?")
		args = append(args, opts.Until)
	}

	whereClause := ""
	if len(where) > 0 {
		whereClause = " WHERE " + strings.Join(where, " AND ")
	}

	summary := &Summary{
		ByBead: make(map[string]int),
		ByRun:  make(map[string]int),
	}

	// Total and reverted counts
	row := s.db.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			COUNT(CASE WHEN reverted_at IS NOT NULL THEN 1 END),
			MIN(landed_at),
			MAX(landed_at)
		FROM landed_changes`+whereClause, args...)

	var firstLanding, lastLanding sql.NullInt64
	if err := row.Scan(&summary.Total, &summary.Reverted, &firstLanding, &lastLanding); err != nil {
		return nil, fmt.Errorf("summary: %w", err)
	}
	if firstLanding.Valid {
		summary.FirstLanding = firstLanding.Int64
	}
	if lastLanding.Valid {
		summary.LastLanding = lastLanding.Int64
	}

	// By bead
	beadFilter := " WHERE bead_id IS NOT NULL AND reverted_at IS NULL"
	if whereClause != "" {
		beadFilter = whereClause + " AND bead_id IS NOT NULL AND reverted_at IS NULL"
	}
	beadRows, err := s.db.QueryContext(ctx, `
		SELECT bead_id, COUNT(*)
		FROM landed_changes`+beadFilter+`
		GROUP BY bead_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("summary by bead: %w", err)
	}
	defer beadRows.Close()
	for beadRows.Next() {
		var beadID string
		var count int
		if err := beadRows.Scan(&beadID, &count); err != nil {
			return nil, fmt.Errorf("summary by bead: scan: %w", err)
		}
		summary.ByBead[beadID] = count
	}
	if err := beadRows.Err(); err != nil {
		return nil, fmt.Errorf("summary by bead: %w", err)
	}

	// By run
	runFilter := " WHERE run_id IS NOT NULL AND reverted_at IS NULL"
	if whereClause != "" {
		runFilter = whereClause + " AND run_id IS NOT NULL AND reverted_at IS NULL"
	}
	runRows, err := s.db.QueryContext(ctx, `
		SELECT run_id, COUNT(*)
		FROM landed_changes`+runFilter+`
		GROUP BY run_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("summary by run: %w", err)
	}
	defer runRows.Close()
	for runRows.Next() {
		var runID string
		var count int
		if err := runRows.Scan(&runID, &count); err != nil {
			return nil, fmt.Errorf("summary by run: scan: %w", err)
		}
		summary.ByRun[runID] = count
	}
	if err := runRows.Err(); err != nil {
		return nil, fmt.Errorf("summary by run: %w", err)
	}

	return summary, nil
}

func nullStr(ns sql.NullString) *string {
	if !ns.Valid {
		return nil
	}
	return &ns.String
}

func nullInt64(ni sql.NullInt64) *int64 {
	if !ni.Valid {
		return nil
	}
	return &ni.Int64
}

func nullInt64Ptr(ni sql.NullInt64) *int64 {
	if !ni.Valid {
		return nil
	}
	return &ni.Int64
}

func nullInt32Ptr(ni sql.NullInt32) *int {
	if !ni.Valid {
		return nil
	}
	v := int(ni.Int32)
	return &v
}
