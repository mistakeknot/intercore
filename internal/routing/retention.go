package routing

import (
	"bufio"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Retention for routing_decisions.
//
// Every routing decision stores its full context JSON (about 5 KB) and nothing
// used to delete a row, so the table grew about 100 MB a month. PruneDecisions
// moves rows older than a window out of the database into a gzip JSONL archive
// and RestoreDecisions puts them back, so a prune is reversible: the archive
// is written and synced to disk before the rows it holds are deleted.
//
// No schema change. A new index on decided_at would bump PRAGMA user_version,
// and every older `ic` binary refuses a database newer than it supports
// (ErrSchemaVersionTooNew), which would break the hooks still running one.
// Instead, rows are visited in id order. AUTOINCREMENT ids and Go-side
// decided_at timestamps grow together, so a binary search over rowids finds
// the end of the old range in O(log n) point lookups without scanning the
// 5 KB context payloads. A run with nothing to prune therefore costs a few
// lookups. The search only narrows where to look: every deleted row is
// re-checked against the cutoff, so an out-of-order timestamp can leave an old
// row for a later run but can never delete a young one.

// DefaultRetention is the retention window for `ic route prune` when none is given.
// Rationale: October 2026 added 19,834 rows and 98 MB, so 30 days holds the table
// near 100 MB instead of growing without bound. The archive keeps the rest.
const DefaultRetention = 30 * 24 * time.Hour

const pruneBatchSize = 500

// PruneOpts configures PruneDecisions.
type PruneOpts struct {
	// OlderThan is the retention window: rows with decided_at older than
	// Now-OlderThan are eligible. Must be positive.
	OlderThan time.Duration
	// Now overrides the clock (tests). Zero means time.Now().
	Now time.Time
	// ArchiveDir receives one gzip JSONL file per calendar month of decided_at
	// (routing-decisions-YYYY-MM.jsonl.gz, appended as gzip members). Empty
	// means no archive, and then rows are deleted for good; callers must ask
	// for that explicitly.
	ArchiveDir string
	// DryRun counts what would be archived and deleted and changes nothing.
	DryRun bool
	// BatchSize limits rows per archive+delete round. Zero means 500.
	BatchSize int
}

// PruneResult reports what a prune did (or, for a dry run, would do).
type PruneResult struct {
	Cutoff     int64    `json:"cutoff"`
	Eligible   int64    `json:"eligible"`
	Archived   int64    `json:"archived"`
	Deleted    int64    `json:"deleted"`
	Protected  int64    `json:"protected"`
	DryRun     bool     `json:"dry_run"`
	ArchiveDir string   `json:"archive_dir,omitempty"`
	Files      []string `json:"files,omitempty"`
}

// A row is protected from pruning, whatever its age, while something live still
// refers to it: a dispatch that has not reached a terminal status (spawn reloads
// its reasoning contract by dispatch_id), a run that is still open, or a terminal
// record that names it as its route decision.
const protectedClause = `(
	(dispatch_id IS NOT NULL AND EXISTS (
		SELECT 1 FROM dispatches d WHERE d.id = routing_decisions.dispatch_id
		AND d.status IN ('spawned','running')))
	OR (run_id IS NOT NULL AND EXISTS (
		SELECT 1 FROM runs r WHERE r.id = routing_decisions.run_id
		AND r.status NOT IN ('completed','cancelled','failed')))
	OR EXISTS (
		SELECT 1 FROM dispatch_terminals t WHERE t.route_decision_id = routing_decisions.id)
)`

// PruneDecisions archives and deletes routing decisions older than the window.
func PruneDecisions(ctx context.Context, db *sql.DB, opts PruneOpts) (*PruneResult, error) {
	if opts.OlderThan <= 0 {
		return nil, errors.New("prune routing decisions: retention window must be positive")
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	batch := opts.BatchSize
	if batch <= 0 {
		batch = pruneBatchSize
	}
	res := &PruneResult{Cutoff: now.Add(-opts.OlderThan).Unix(), DryRun: opts.DryRun}

	bound, err := oldRangeEnd(ctx, db, res.Cutoff)
	if err != nil {
		return nil, err
	}
	if bound == 0 {
		return res, nil
	}

	if opts.ArchiveDir != "" && !opts.DryRun {
		if err := os.MkdirAll(opts.ArchiveDir, 0o700); err != nil {
			return nil, fmt.Errorf("prune routing decisions: archive dir: %w", err)
		}
		res.ArchiveDir = opts.ArchiveDir
	}

	files := map[string]bool{}
	var after int64
	for {
		rows, err := selectOldBatch(ctx, db, after, bound, res.Cutoff, batch)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			break
		}
		after = rows[len(rows)-1].ID
		res.Eligible += int64(len(rows))
		if opts.DryRun {
			continue
		}
		if opts.ArchiveDir != "" {
			written, err := appendArchive(opts.ArchiveDir, rows)
			if err != nil {
				return nil, fmt.Errorf("prune routing decisions: archive: %w (nothing in this batch was deleted)", err)
			}
			for _, f := range written {
				files[f] = true
			}
			res.Archived += int64(len(rows))
		}
		n, err := deleteBatch(ctx, db, rows)
		if err != nil {
			return nil, err
		}
		res.Deleted += n
	}

	// Rows that are old but still referenced stay; report how many.
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM routing_decisions
		WHERE id <= ? AND decided_at < ? AND `+protectedClause, bound, res.Cutoff).Scan(&res.Protected); err != nil {
		return nil, fmt.Errorf("prune routing decisions: count protected: %w", err)
	}
	for f := range files {
		res.Files = append(res.Files, f)
	}
	sort.Strings(res.Files)
	return res, nil
}

// oldRangeEnd returns the largest id whose decided_at is before cutoff, found
// by binary search over rowids, or 0 when the table has no old rows. It assumes
// decided_at rises with id; see the package comment for why that is safe.
func oldRangeEnd(ctx context.Context, db *sql.DB, cutoff int64) (int64, error) {
	var lo, hi sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT MIN(id), MAX(id) FROM routing_decisions`).Scan(&lo, &hi); err != nil {
		return 0, fmt.Errorf("prune routing decisions: id range: %w", err)
	}
	if !lo.Valid {
		return 0, nil
	}
	// decidedAt returns decided_at of the first row with id >= at.
	decidedAt := func(at int64) (int64, int64, bool, error) {
		var id, ts int64
		err := db.QueryRowContext(ctx, `SELECT id, decided_at FROM routing_decisions WHERE id >= ? ORDER BY id LIMIT 1`, at).Scan(&id, &ts)
		if err == sql.ErrNoRows {
			return 0, 0, false, nil
		}
		return id, ts, err == nil, err
	}
	if _, ts, ok, err := decidedAt(lo.Int64); err != nil {
		return 0, fmt.Errorf("prune routing decisions: probe: %w", err)
	} else if !ok || ts >= cutoff {
		return 0, nil
	}
	// Invariant: the row at lo is old. Find the last old row at or after lo.
	last := lo.Int64
	l, h := lo.Int64, hi.Int64
	for l <= h {
		mid := l + (h-l)/2
		id, ts, ok, err := decidedAt(mid)
		if err != nil {
			return 0, fmt.Errorf("prune routing decisions: probe: %w", err)
		}
		if !ok {
			h = mid - 1
			continue
		}
		if ts < cutoff {
			last = id
			l = id + 1
		} else {
			h = mid - 1
		}
	}
	return last, nil
}

func selectOldBatch(ctx context.Context, db *sql.DB, after, bound, cutoff int64, limit int) ([]Decision, error) {
	q := `SELECT id, dispatch_id, run_id, session_id, bead_id,
		project_dir, phase, agent, category,
		selected_model, rule_matched,
		floor_applied, floor_from, floor_to,
		candidates, excluded,
		policy_hash, override_id, complexity, context_json,
		decided_at
		FROM routing_decisions
		WHERE id > ? AND id <= ? AND decided_at < ? AND NOT ` + protectedClause + `
		ORDER BY id LIMIT ?`
	rs, err := db.QueryContext(ctx, q, after, bound, cutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("prune routing decisions: select: %w", err)
	}
	defer rs.Close()
	var out []Decision
	for rs.Next() {
		var (
			d                                                     Decision
			dispatchID, runID, sessionID, beadID, phase, category sql.NullString
			floorFrom, floorTo, candidates, excluded, policyHash  sql.NullString
			overrideID, contextJSON                               sql.NullString
			complexity                                            sql.NullInt64
			floorApplied                                          int
		)
		if err := rs.Scan(&d.ID, &dispatchID, &runID, &sessionID, &beadID,
			&d.ProjectDir, &phase, &d.Agent, &category,
			&d.SelectedModel, &d.RuleMatched,
			&floorApplied, &floorFrom, &floorTo,
			&candidates, &excluded,
			&policyHash, &overrideID, &complexity, &contextJSON,
			&d.DecidedAt); err != nil {
			return nil, fmt.Errorf("prune routing decisions: scan: %w", err)
		}
		d.DispatchID, d.RunID, d.SessionID, d.BeadID = decNullStr(dispatchID), decNullStr(runID), decNullStr(sessionID), decNullStr(beadID)
		d.Phase, d.Category = decNullStr(phase), decNullStr(category)
		d.FloorApplied = floorApplied != 0
		d.FloorFrom, d.FloorTo = decNullStr(floorFrom), decNullStr(floorTo)
		d.Candidates, d.Excluded = decNullStr(candidates), decNullStr(excluded)
		d.PolicyHash, d.OverrideID = decNullStr(policyHash), decNullStr(overrideID)
		d.Complexity = decNullIntPtr(complexity)
		d.ContextJSON = decNullStr(contextJSON)
		out = append(out, d)
	}
	return out, rs.Err()
}

func deleteBatch(ctx context.Context, db *sql.DB, rows []Decision) (int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("prune routing decisions: begin: %w", err)
	}
	defer tx.Rollback()
	var total int64
	for _, r := range rows {
		// Re-check the age here so a row can never be deleted on the strength of
		// the search alone.
		out, err := tx.ExecContext(ctx, `DELETE FROM routing_decisions WHERE id = ? AND decided_at = ?`, r.ID, r.DecidedAt)
		if err != nil {
			return 0, fmt.Errorf("prune routing decisions: delete: %w", err)
		}
		n, _ := out.RowsAffected()
		total += n
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("prune routing decisions: commit: %w", err)
	}
	return total, nil
}

// archiveName is the archive file for the month a row was decided in (UTC).
func archiveName(decidedAt int64) string {
	return "routing-decisions-" + time.Unix(decidedAt, 0).UTC().Format("2006-01") + ".jsonl.gz"
}

// appendArchive appends rows to their monthly archive files, one gzip member per
// call and file, and syncs each file before returning.
func appendArchive(dir string, rows []Decision) ([]string, error) {
	byFile := map[string][]Decision{}
	for _, r := range rows {
		n := archiveName(r.DecidedAt)
		byFile[n] = append(byFile[n], r)
	}
	var names []string
	for n := range byFile {
		names = append(names, n)
	}
	sort.Strings(names)
	var written []string
	for _, n := range names {
		path := filepath.Join(dir, n)
		if err := appendMember(path, byFile[n]); err != nil {
			return nil, err
		}
		written = append(written, path)
	}
	return written, nil
}

// appendMember appends one gzip member holding rows to path and syncs it. If the
// write fails part-way the file is cut back to its earlier length, so a failed
// batch cannot leave a truncated member that blocks the members after it.
func appendMember(path string, rows []Decision) (err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	prev := st.Size()
	defer func() {
		if err != nil {
			_ = f.Truncate(prev)
		}
	}()
	zw := gzip.NewWriter(f)
	enc := json.NewEncoder(zw)
	for _, r := range rows {
		if err = enc.Encode(r); err != nil {
			return err
		}
	}
	if err = zw.Close(); err != nil {
		return err
	}
	return f.Sync()
}

// RestoreResult reports what RestoreDecisions did.
type RestoreResult struct {
	Read     int64 `json:"read"`
	Restored int64 `json:"restored"`
	Existing int64 `json:"already_present"`
}

// RestoreDecisions reads archive files (gzip JSONL as written by PruneDecisions)
// and re-inserts their rows with their original ids. Rows whose id is already
// present are left alone, so restoring twice, or restoring a file that holds
// rows from an interrupted prune, is harmless.
func RestoreDecisions(ctx context.Context, db *sql.DB, paths []string) (*RestoreResult, error) {
	res := &RestoreResult{}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("restore routing decisions: begin: %w", err)
	}
	defer tx.Rollback()
	for _, p := range paths {
		if err := restoreFile(ctx, tx, p, res); err != nil {
			return nil, fmt.Errorf("restore routing decisions: %s: %w", p, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("restore routing decisions: commit: %w", err)
	}
	return res, nil
}

func restoreFile(ctx context.Context, tx *sql.Tx, path string, res *RestoreResult) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var src io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		zr, err := gzip.NewReader(f) // reads concatenated members
		if err != nil {
			return err
		}
		defer zr.Close()
		src = zr
	}
	sc := bufio.NewReaderSize(src, 1<<20)
	for {
		line, err := sc.ReadBytes('\n')
		if len(line) > 0 {
			var d Decision
			if jerr := json.Unmarshal(line, &d); jerr != nil {
				return fmt.Errorf("line %d: %w", res.Read+1, jerr)
			}
			res.Read++
			fa := 0
			if d.FloorApplied {
				fa = 1
			}
			out, ierr := tx.ExecContext(ctx, `INSERT OR IGNORE INTO routing_decisions (
				id, dispatch_id, run_id, session_id, bead_id, project_dir, phase, agent, category,
				selected_model, rule_matched, floor_applied, floor_from, floor_to,
				candidates, excluded, policy_hash, override_id, complexity, context_json, decided_at)
				VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				d.ID, d.DispatchID, d.RunID, d.SessionID, d.BeadID, d.ProjectDir, d.Phase, d.Agent, d.Category,
				d.SelectedModel, d.RuleMatched, fa, d.FloorFrom, d.FloorTo,
				d.Candidates, d.Excluded, d.PolicyHash, d.OverrideID, d.Complexity, d.ContextJSON, d.DecidedAt)
			if ierr != nil {
				return ierr
			}
			if n, _ := out.RowsAffected(); n > 0 {
				res.Restored++
			} else {
				res.Existing++
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
