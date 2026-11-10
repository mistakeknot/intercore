package routing

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
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
// moves rows older than a window out of the database into gzip JSONL archive
// files and RestoreDecisions puts them back, so a prune is reversible: a batch's
// archive file is complete, synced and renamed into place, and its directory
// entry synced, before any row of that batch is deleted.
//
// Each batch is its own file and nothing is ever appended to a file, so an
// interrupted run can only leave an unreferenced temp file, never a damaged
// archive, and two prunes at once cannot interleave their output. File names carry
// the id range and a hash of the content, so re-archiving the same rows after a
// crash between archive and delete writes the same name.
//
// No schema change. A new index on decided_at would bump PRAGMA user_version,
// and every older `ic` binary refuses a database newer than it supports
// (ErrSchemaVersionTooNew), which would break the hooks still running one. So
// rows are visited in id order with decided_at tested on every row. That reads
// the context payloads (decided_at is stored after them) and costs about a second
// on a 100 MB table, which is why the hook runs the prune once a day after a
// success, not on every stop. It makes no assumption that timestamps rise with ids.

// DefaultRetention is the retention window for `ic route prune` when none is given.
// Rationale: October 2026 added 19,834 rows and 98 MB, so 30 days holds the table
// near 100 MB instead of growing without bound. The archive keeps the rest.
const DefaultRetention = 30 * 24 * time.Hour

const pruneBatchSize = 2000

// PruneOpts configures PruneDecisions.
type PruneOpts struct {
	// OlderThan is the retention window: rows with decided_at older than
	// Now-OlderThan are eligible. Must be positive.
	OlderThan time.Duration
	// Now overrides the clock (tests). Zero means time.Now().
	Now time.Time
	// ArchiveDir receives, per batch and calendar month of decided_at (UTC), one
	// gzip JSONL file named routing-decisions-YYYY-MM-<minid>-<maxid>-<hash>.jsonl.gz.
	// Empty means no archive, and then rows are deleted for good; callers must ask
	// for that explicitly.
	ArchiveDir string
	// DryRun counts what would be archived and deleted and changes nothing.
	DryRun bool
	// BatchSize limits rows per archive+delete round. Zero means 2000.
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

	if opts.ArchiveDir != "" && !opts.DryRun {
		if err := mkdirAllSync(opts.ArchiveDir); err != nil {
			return nil, fmt.Errorf("prune routing decisions: archive dir: %w", err)
		}
		res.ArchiveDir = opts.ArchiveDir
	}

	files := map[string]bool{}
	var after int64
	for {
		old, err := selectOldBatch(ctx, db, after, res.Cutoff, batch)
		if err != nil {
			return nil, err
		}
		if len(old) == 0 {
			break
		}
		after = old[len(old)-1].ID
		var rows []Decision
		for _, d := range old {
			if d.protected {
				res.Protected++
			} else {
				rows = append(rows, d.Decision)
			}
		}
		res.Eligible += int64(len(rows))
		if opts.DryRun || len(rows) == 0 {
			continue
		}
		if opts.ArchiveDir != "" {
			written, err := writeArchive(opts.ArchiveDir, rows)
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
	for f := range files {
		res.Files = append(res.Files, f)
	}
	sort.Strings(res.Files)
	return res, nil
}

// oldDecision is an old row and whether something live still refers to it.
type oldDecision struct {
	Decision
	protected bool
}

// selectOldBatch returns the next old rows after id `after`, in id order. The age
// test is on every row, so ids and timestamps need not agree.
func selectOldBatch(ctx context.Context, db *sql.DB, after, cutoff int64, limit int) ([]oldDecision, error) {
	q := `SELECT id, dispatch_id, run_id, session_id, bead_id,
		project_dir, phase, agent, category,
		selected_model, rule_matched,
		floor_applied, floor_from, floor_to,
		candidates, excluded,
		policy_hash, override_id, complexity, context_json,
		decided_at, ` + protectedClause + `
		FROM routing_decisions
		WHERE id > ? AND decided_at < ?
		ORDER BY id LIMIT ?`
	rs, err := db.QueryContext(ctx, q, after, cutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("prune routing decisions: select: %w", err)
	}
	defer rs.Close()
	var out []oldDecision
	for rs.Next() {
		var (
			d                                                     Decision
			prot                                                  int
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
			&d.DecidedAt, &prot); err != nil {
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
		out = append(out, oldDecision{Decision: d, protected: prot != 0})
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
		// Re-check, inside the deleting statement, that the row is unchanged and
		// still unreferenced: a dispatch, run or terminal record may have come to
		// point at it since it was selected and archived. Such a row stays (its
		// archive copy is then a harmless duplicate).
		out, err := tx.ExecContext(ctx, `DELETE FROM routing_decisions WHERE id = ? AND decided_at = ? AND NOT `+protectedClause, r.ID, r.DecidedAt)
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

// writeArchive writes rows to archive files, one per UTC month of decided_at, each
// complete, synced and renamed into place with its directory entry synced before
// it returns. It returns the final paths.
func writeArchive(dir string, rows []Decision) ([]string, error) {
	byMonth := map[string][]Decision{}
	for _, r := range rows {
		m := time.Unix(r.DecidedAt, 0).UTC().Format("2006-01")
		byMonth[m] = append(byMonth[m], r)
	}
	var months []string
	for m := range byMonth {
		months = append(months, m)
	}
	sort.Strings(months)
	var written []string
	for _, m := range months {
		path, err := writeArchiveFile(dir, m, byMonth[m])
		if err != nil {
			return nil, err
		}
		written = append(written, path)
	}
	return written, nil
}

func writeArchiveFile(dir, month string, rows []Decision) (string, error) {
	var plain bytes.Buffer
	enc := json.NewEncoder(&plain)
	minID, maxID := rows[0].ID, rows[0].ID
	for _, r := range rows {
		if err := enc.Encode(r); err != nil {
			return "", err
		}
		if r.ID < minID {
			minID = r.ID
		}
		if r.ID > maxID {
			maxID = r.ID
		}
	}
	sum := sha256.Sum256(plain.Bytes())
	final := filepath.Join(dir, fmt.Sprintf("routing-decisions-%s-%d-%d-%s.jsonl.gz", month, minID, maxID, hex.EncodeToString(sum[:4])))

	tmp, err := os.CreateTemp(dir, ".routing-decisions-*.tmp") // mode 0600
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = tmp.Close(); _ = os.Remove(tmpName) }
	zw := gzip.NewWriter(tmp)
	if _, err := zw.Write(plain.Bytes()); err != nil {
		cleanup()
		return "", err
	}
	if err := zw.Close(); err != nil {
		cleanup()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", err
	}
	// The name holds a hash of the content, so a file already there is the same
	// archive (a rerun after a crash before the delete); replacing it is harmless.
	if err := os.Rename(tmpName, final); err != nil {
		_ = os.Remove(tmpName)
		return "", err
	}
	if err := syncDir(dir); err != nil {
		return "", err
	}
	return final, nil
}

// syncDir makes the directory's entries durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// mkdirAllSync creates dir and any missing parents and syncs each parent that
// gained an entry, so the archive directory itself survives a power loss.
func mkdirAllSync(dir string) error {
	dir = filepath.Clean(dir)
	var created []string
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(d); err == nil {
			break
		}
		created = append(created, d)
		if filepath.Dir(d) == d {
			break
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for _, d := range created {
		if err := syncDir(filepath.Dir(d)); err != nil {
			return err
		}
	}
	return nil
}

// RestoreResult reports what RestoreDecisions did.
type RestoreResult struct {
	Read     int64 `json:"read"`
	Restored int64 `json:"restored"`
	Existing int64 `json:"already_present"`
}

// expandArchivePaths replaces each directory with the archive files in it
// (*.jsonl.gz and *.jsonl, in name order; temp files are not archives).
func expandArchivePaths(paths []string) ([]string, error) {
	var out []string
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if !st.IsDir() {
			out = append(out, p)
			continue
		}
		var found []string
		for _, pat := range []string{"*.jsonl.gz", "*.jsonl"} {
			m, err := filepath.Glob(filepath.Join(p, pat))
			if err != nil {
				return nil, err
			}
			found = append(found, m...)
		}
		sort.Strings(found)
		out = append(out, found...)
	}
	return out, nil
}

// RestoreDecisions reads archive files, or directories of them (gzip JSONL as
// written by PruneDecisions), and re-inserts their rows with their original ids.
// It is all or nothing: a damaged file fails the whole restore and changes nothing. Rows whose id is already
// present are left alone, so restoring twice, or restoring a file that holds
// rows from an interrupted prune, is harmless.
func RestoreDecisions(ctx context.Context, db *sql.DB, paths []string) (*RestoreResult, error) {
	res := &RestoreResult{}
	paths, err := expandArchivePaths(paths)
	if err != nil {
		return nil, fmt.Errorf("restore routing decisions: %w", err)
	}
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
			if d.ID <= 0 || d.DecidedAt <= 0 || d.SelectedModel == "" {
				return fmt.Errorf("line %d: not a routing decision record (id %d, decided_at %d, model %q)", res.Read+1, d.ID, d.DecidedAt, d.SelectedModel)
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
