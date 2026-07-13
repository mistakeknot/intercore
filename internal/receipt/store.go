package receipt

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNotFound is returned by Store.Get when receipt_id is unknown.
var ErrNotFound = errors.New("receipt not found")

// ErrUnsigned is returned by Store.Insert when the receipt envelope is
// incomplete — i.e., Sign was never called or the receipt was tampered with
// to clear the envelope. The schema requires non-empty signature, key_id,
// signature_alg, and signed_at.
var ErrUnsigned = errors.New("receipt envelope incomplete (sign first)")

// Store persists signed receipts in the intercore SQLite database under
// table action_receipts. Schema is owned by db migration 035 and is
// INSERT-only: UPDATE and DELETE both abort via triggers per canon
// §Trust claim.
//
// Store is safe for concurrent use through the *sql.DB connection pool.
// In intercore the pool is configured with SetMaxOpenConns(1) per the
// package-level CLAUDE.md constraint, so writes serialize naturally.
type Store struct {
	db *sql.DB
}

// NewStore wraps a *sql.DB. The caller is responsible for opening the DB
// and running migrations (db.Migrator handles 034→035 transparently).
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// Insert persists a signed receipt. The caller MUST have called Sign on r
// before passing it in (the envelope fields are checked). payloadCanonical
// is the byte slice returned by Sign — storing it avoids re-canonicalization
// at verify time and lets later auditors detect canonicalization-rule drift.
//
// If payloadCanonical is nil, Insert calls Canonicalize to produce one. The
// common path is to pass the value Sign returned, which is byte-identical.
//
// On the INSERT-only schema, attempting to Insert a duplicate receipt_id
// returns the underlying constraint error (PK violation).
func (s *Store) Insert(ctx context.Context, r *Receipt, payloadCanonical []byte) error {
	if r.Signature == "" || r.SignatureAlg == "" || r.KeyID == "" || r.SignedAt == "" {
		return ErrUnsigned
	}
	if payloadCanonical == nil {
		payloadCanonical = Canonicalize(r)
	}
	toolCallsJSON := string(CanonicalizeToolCalls(r.ToolCalls))

	const stmt = `INSERT INTO action_receipts (
		receipt_id, timestamp, agent_id, model, tool_calls_json, parent_run_id,
		content_hash, schema_version, signature, signature_alg, key_id, signed_at,
		payload_canonical, inserted_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	var parent any // sql treats nil as NULL; *string maps cleanly
	if r.ParentRunID != nil {
		parent = *r.ParentRunID
	}

	_, err := s.db.ExecContext(ctx, stmt,
		r.ReceiptID,
		r.Timestamp,
		r.AgentID,
		r.Model,
		toolCallsJSON,
		parent,
		r.ContentHash,
		r.SchemaVersion,
		r.Signature,
		r.SignatureAlg,
		r.KeyID,
		r.SignedAt,
		payloadCanonical,
		time.Now().UTC().Unix(),
	)
	if err != nil {
		return fmt.Errorf("insert receipt %s: %w", r.ReceiptID, err)
	}
	return nil
}

// Get returns the receipt + its stored payload_canonical for a given
// receipt_id. The payload is the exact byte stream Sign produced; callers
// MAY pass it to a verifier directly (avoiding any canonicalization-rule
// drift between the writer and the verifier).
//
// Returns ErrNotFound if receipt_id is unknown.
func (s *Store) Get(ctx context.Context, receiptID string) (*Receipt, []byte, error) {
	const q = `SELECT
		receipt_id, timestamp, agent_id, model, tool_calls_json, parent_run_id,
		content_hash, schema_version, signature, signature_alg, key_id, signed_at,
		payload_canonical
	  FROM action_receipts WHERE receipt_id = ?`
	row := s.db.QueryRowContext(ctx, q, receiptID)
	r, payload, err := scanRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, fmt.Errorf("%w: %s", ErrNotFound, receiptID)
	}
	return r, payload, err
}

// FindAction returns the earliest signed receipt for one logical action.
// The exact tuple is Remontoire's idempotency boundary for recovering an emit
// that may have committed before its process lost the receipt ID.
func (s *Store) FindAction(ctx context.Context, agentID, parentRunID, contentHash string) (*Receipt, error) {
	const q = `SELECT
		receipt_id, timestamp, agent_id, model, tool_calls_json, parent_run_id,
		content_hash, schema_version, signature, signature_alg, key_id, signed_at,
		payload_canonical
	  FROM action_receipts
	 WHERE agent_id = ? AND parent_run_id = ? AND content_hash = ?
	 ORDER BY timestamp ASC, receipt_id ASC
	 LIMIT 1`
	row := s.db.QueryRowContext(ctx, q, agentID, parentRunID, contentHash)
	r, _, err := scanRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: action %s/%s", ErrNotFound, agentID, parentRunID)
	}
	if err != nil {
		return nil, fmt.Errorf("find action receipt: %w", err)
	}
	return r, nil
}

// ListOpts filters a List call. Zero-value fields are ignored.
type ListOpts struct {
	AgentID string
	Since   time.Time // matches `timestamp >= Since` as canonical RFC3339 string
	Limit   int       // 0 means no limit
}

// List returns receipts matching opts, ordered by timestamp ascending.
// Payload bytes are NOT returned by List to keep the working set small;
// callers that need them should follow up with Get on specific receipt_ids.
func (s *Store) List(ctx context.Context, opts ListOpts) ([]*Receipt, error) {
	var (
		where []string
		args  []any
	)
	if opts.AgentID != "" {
		where = append(where, "agent_id = ?")
		args = append(args, opts.AgentID)
	}
	if !opts.Since.IsZero() {
		where = append(where, "timestamp >= ?")
		args = append(args, FormatTimestamp(opts.Since))
	}
	clause := ""
	if len(where) > 0 {
		clause = " WHERE " + strings.Join(where, " AND ")
	}
	q := `SELECT
		receipt_id, timestamp, agent_id, model, tool_calls_json, parent_run_id,
		content_hash, schema_version, signature, signature_alg, key_id, signed_at,
		payload_canonical
	  FROM action_receipts` + clause + ` ORDER BY timestamp ASC`
	if opts.Limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", opts.Limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list receipts: %w", err)
	}
	defer rows.Close()
	var out []*Receipt
	for rows.Next() {
		r, _, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows so scanRow serves
// both Get and List without duplicating the column ordering.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanRow(row rowScanner) (*Receipt, []byte, error) {
	var (
		r             Receipt
		toolCallsJSON string
		parent        sql.NullString
		payload       []byte
	)
	err := row.Scan(
		&r.ReceiptID, &r.Timestamp, &r.AgentID, &r.Model, &toolCallsJSON, &parent,
		&r.ContentHash, &r.SchemaVersion, &r.Signature, &r.SignatureAlg, &r.KeyID, &r.SignedAt,
		&payload,
	)
	if err != nil {
		return nil, nil, err
	}
	if parent.Valid {
		p := parent.String
		r.ParentRunID = &p
	}
	calls, err := parseToolCallsJSON(toolCallsJSON)
	if err != nil {
		return nil, nil, fmt.Errorf("parse tool_calls for %s: %w", r.ReceiptID, err)
	}
	r.ToolCalls = calls
	return &r, payload, nil
}

// ParseToolCallsJSON decodes a JSON tool_calls array into []ToolCall. Exported
// for the `ic receipt emit` CLI, which accepts tool_calls as a JSON argument.
func ParseToolCallsJSON(s string) ([]ToolCall, error) {
	return parseToolCallsJSON(s)
}

// parseToolCallsJSON decodes the tool_calls_json column. We use the standard
// library decoder here (not the canonical encoder) because reads do not need
// to match canonical-byte output — only writes do.
//
// The canonical JSON format is a strict subset of standard JSON, so
// encoding/json parses it without trouble.
func parseToolCallsJSON(s string) ([]ToolCall, error) {
	if s == "" || s == "[]" {
		return nil, nil
	}
	type wireCall struct {
		Name       string `json:"name"`
		ArgsHash   string `json:"args_hash"`
		ResultHash string `json:"result_hash"`
		DurationMs int64  `json:"duration_ms"`
	}
	var w []wireCall
	if err := json.Unmarshal([]byte(s), &w); err != nil {
		return nil, err
	}
	out := make([]ToolCall, len(w))
	for i, c := range w {
		out[i] = ToolCall{Name: c.Name, ArgsHash: c.ArgsHash, ResultHash: c.ResultHash, DurationMs: c.DurationMs}
	}
	return out, nil
}
