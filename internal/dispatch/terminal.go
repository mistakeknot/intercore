package dispatch

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// ErrAlreadyTerminal reports that the dispatch was already terminal when a
// writer tried to record its outcome. It wraps ErrStaleStatus, so callers that
// treat a lost race as collected keep working. Terminalize returns the existing
// record with it; that record is nil for a dispatch that ended before schema 40.
var ErrAlreadyTerminal = fmt.Errorf("%w: dispatch already terminal", ErrStaleStatus)

// Terminal sources a writer may record. The outbox trigger records "trigger";
// "legacy" is reserved.
const (
	TerminalSourceSupervisor  = "supervisor"
	TerminalSourceReconcile   = "reconcile"
	TerminalSourceCollect     = "collect"
	TerminalSourceSpawn       = "spawn"
	TerminalSourceCancelByRun = "cancel_by_run"
	TerminalSourceKill        = "kill"
	TerminalSourceSpool       = "spool"
)

// Evidence states of a terminal record. Only the outbox trigger records
// EvidenceUnrecorded.
const (
	EvidenceVerified      = "verified"
	EvidenceMissing       = "missing"
	EvidenceMalformed     = "malformed"
	EvidenceMismatched    = "mismatched"
	EvidenceNotApplicable = "not_applicable"
	EvidenceUnrecorded    = "unrecorded"
)

// Usage states of a terminal record.
const (
	UsageComplete   = "complete"
	UsageIncomplete = "incomplete"
	UsageUnknown    = "unknown"
)

// ErrSupervised reports that a dispatch has a supervisor, the only writer of its
// outcome while it lives (contract I7). Collect, spawn, kill and run-rollback
// writers refuse it; the supervisor, reconciliation after the supervisor is
// proven dead, and spool import record it.
var ErrSupervised = errors.New("dispatch is supervised: only its supervisor records the outcome")

var (
	errTerminalRecordExists = errors.New("terminal record already exists")
	errTerminalCASMissed    = errors.New("dispatch became terminal during the write")
)

func supervisedWriter(source string) bool {
	switch source {
	case TerminalSourceSupervisor, TerminalSourceReconcile, TerminalSourceSpool:
		return true
	}
	return false
}

// isSupervised reports whether dispatch id has a supervision record.
func isSupervised(ctx context.Context, q querier, id string) (bool, error) {
	var supervised bool
	err := q.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM dispatch_supervision WHERE dispatch_id = ?)", id).Scan(&supervised)
	if err != nil {
		return false, fmt.Errorf("read supervision: %w", err)
	}
	return supervised, nil
}

// querier is what the terminal writer needs from *sql.Conn, *sql.Tx or *sql.DB.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Terminal is an outcome to record: the dispatch's terminal status, the
// dispatch columns written with it, and the terminal record's own fields.
// Compute every field before calling Terminalize, which holds the pool's only
// connection while it writes.
type Terminal struct {
	Status   string
	Source   string
	Evidence string
	// Reason is stored on the terminal event.
	Reason string
	// Fields are dispatch columns written with the status; see allowedUpdateCols.
	Fields UpdateFields

	ExitCode           *int
	ExitSignal         string
	FailureClass       string
	UsageStatus        string // empty records unknown
	InputTokens        *int64
	OutputTokens       *int64
	CacheReadTokens    *int64
	CacheWriteTokens   *int64
	ProducerBackend    string
	ProducerModel      string
	RouteDecisionID    *int64
	ReportPath         string
	ReportSHA256       string
	ArtifactsJSON      string // empty records {}
	BaseCommit         string
	BaseWorktreeDigest string
	HeadCommit         string
	HeadWorktreeDigest string
	OrphansKilled      int
}

// TerminalRecord is a row of dispatch_terminals.
type TerminalRecord struct {
	DispatchID         string
	RunID              string
	Attempt            int
	ParentDispatchID   string
	Status             string
	FromStatus         string
	Source             string
	ExitCode           *int
	ExitSignal         string
	FailureClass       string
	Evidence           string
	UsageStatus        string
	InputTokens        *int64
	OutputTokens       *int64
	CacheReadTokens    *int64
	CacheWriteTokens   *int64
	ProducerBackend    string
	ProducerModel      string
	RouteDecisionID    *int64
	ReportPath         string
	ReportSHA256       string
	ArtifactsJSON      string
	BaseCommit         string
	BaseWorktreeDigest string
	HeadCommit         string
	HeadWorktreeDigest string
	OrphansKilled      int
	Contested          bool
	EventID            int64
	CreatedAt          int64
}

func (t *Terminal) validate() error {
	if !isTerminalStatus(t.Status) {
		return fmt.Errorf("terminalize: %q is not a terminal status", t.Status)
	}
	switch t.Source {
	case TerminalSourceSupervisor, TerminalSourceReconcile, TerminalSourceCollect, TerminalSourceSpawn,
		TerminalSourceCancelByRun, TerminalSourceKill, TerminalSourceSpool:
	default:
		return fmt.Errorf("terminalize: %q is not a writer terminal source", t.Source)
	}
	switch t.Evidence {
	case EvidenceVerified, EvidenceMissing, EvidenceMalformed, EvidenceMismatched, EvidenceNotApplicable:
	default:
		return fmt.Errorf("terminalize: %q is not a writer evidence status", t.Evidence)
	}
	switch t.UsageStatus {
	case "", UsageComplete, UsageIncomplete, UsageUnknown:
	default:
		return fmt.Errorf("terminalize: %q is not a usage status", t.UsageStatus)
	}
	for col := range t.Fields {
		if !allowedUpdateCols[col] {
			return fmt.Errorf("terminalize: disallowed column: %q", col)
		}
	}
	return nil
}

// Terminalize records t as the outcome of dispatch id in one immediate
// transaction and returns the committed record. If the dispatch was already
// terminal it returns ErrAlreadyTerminal with the existing record.
func (s *Store) Terminalize(ctx context.Context, id string, t Terminal) (*TerminalRecord, error) {
	if err := t.validate(); err != nil {
		return nil, err
	}
	trace := traceFromEnv()
	var (
		rec  *TerminalRecord
		prev string
	)
	err := withImmediateTx(ctx, s.db, "terminalize", func(conn *sql.Conn) error {
		var err error
		rec, prev, err = terminalizeTx(ctx, conn, id, t, trace)
		return err
	})
	if errors.Is(err, errTerminalRecordExists) || errors.Is(err, errTerminalCASMissed) {
		existing, rerr := readTerminal(ctx, s.db, id)
		if rerr != nil {
			return nil, rerr
		}
		return existing, ErrAlreadyTerminal
	}
	if err != nil {
		return rec, err
	}
	// Legacy consumers key on status_change; it stays post-commit and best effort.
	if s.eventRecorder != nil {
		s.eventRecorder(id, rec.RunID, prev, t.Status)
	}
	return rec, nil
}

// terminalizeTx records t for dispatch id inside the caller's immediate
// transaction, in the contract's order (§5.1): the terminal event, the terminal
// record naming it, then the status change guarded against another terminal
// write. It uses only q, so it never waits on the pool's single connection. It
// returns the record and the dispatch's previous status.
func terminalizeTx(ctx context.Context, q querier, id string, t Terminal, trace traceContext) (*TerminalRecord, string, error) {
	var (
		prev, parentID       string
		attempt              int
		scopeID              sql.NullString
		requested, effective sql.NullString
	)
	err := q.QueryRowContext(ctx,
		"SELECT status, retry_count, scope_id, parent_dispatch_id, sandbox_spec, sandbox_effective FROM dispatches WHERE id = ?",
		id).Scan(&prev, &attempt, &scopeID, &parentID, &requested, &effective)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", fmt.Errorf("terminalize: read dispatch: %w", err)
	}
	if isTerminalStatus(prev) {
		existing, err := readTerminal(ctx, q, id)
		if err != nil {
			return nil, prev, err
		}
		return existing, prev, ErrAlreadyTerminal
	}
	if !supervisedWriter(t.Source) {
		supervised, err := isSupervised(ctx, q, id)
		if err != nil {
			return nil, prev, fmt.Errorf("terminalize: %w", err)
		}
		if supervised {
			return nil, prev, ErrSupervised
		}
	}
	runID := scopeID.String
	if v, ok := t.Fields["sandbox_effective"].(string); ok {
		effective = sql.NullString{String: v, Valid: true}
	}

	envelope, err := json.Marshal(trace.envelope(id, runID, prev, t.Status, requested.String, effective.String))
	if err != nil {
		return nil, prev, fmt.Errorf("terminalize: envelope: %w", err)
	}
	res, err := q.ExecContext(ctx, `
		INSERT INTO dispatch_events (dispatch_id, run_id, from_status, to_status, event_type, reason, envelope_json)
		VALUES (?, NULLIF(?, ''), ?, ?, 'terminal', NULLIF(?, ''), ?)`,
		id, runID, prev, t.Status, t.Reason, string(envelope))
	if err != nil {
		return nil, prev, fmt.Errorf("terminalize: event: %w", err)
	}
	eventID, err := res.LastInsertId()
	if err != nil {
		return nil, prev, fmt.Errorf("terminalize: event id: %w", err)
	}

	usage := t.UsageStatus
	if usage == "" {
		usage = UsageUnknown
	}
	artifacts := t.ArtifactsJSON
	if artifacts == "" {
		artifacts = "{}"
	}
	_, err = q.ExecContext(ctx, `
		INSERT INTO dispatch_terminals (
			dispatch_id, run_id, attempt, parent_dispatch_id, status, from_status, terminal_source,
			exit_code, exit_signal, failure_class, evidence_status, usage_status,
			input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
			producer_backend, producer_model, route_decision_id, report_path, report_sha256, artifacts_json,
			base_commit, base_worktree_digest, head_commit, head_worktree_digest,
			orphans_killed, event_id, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, runID, attempt, parentID, t.Status, prev, t.Source,
		t.ExitCode, nullIfEmpty(t.ExitSignal), nullIfEmpty(t.FailureClass), t.Evidence, usage,
		t.InputTokens, t.OutputTokens, t.CacheReadTokens, t.CacheWriteTokens,
		nullIfEmpty(t.ProducerBackend), nullIfEmpty(t.ProducerModel), t.RouteDecisionID,
		nullIfEmpty(t.ReportPath), nullIfEmpty(t.ReportSHA256), artifacts,
		nullIfEmpty(t.BaseCommit), nullIfEmpty(t.BaseWorktreeDigest), nullIfEmpty(t.HeadCommit), nullIfEmpty(t.HeadWorktreeDigest),
		t.OrphansKilled, eventID, time.Now().Unix())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: dispatch_terminals.dispatch_id") {
			return nil, prev, errTerminalRecordExists
		}
		return nil, prev, fmt.Errorf("terminalize: record: %w", err)
	}

	sets := []string{"status = ?"}
	args := []any{t.Status}
	for col, val := range t.Fields {
		sets = append(sets, col+" = ?")
		args = append(args, val)
	}
	args = append(args, id)
	res, err = q.ExecContext(ctx,
		"UPDATE dispatches SET "+strings.Join(sets, ", ")+" WHERE id = ? AND status NOT IN ('completed', 'failed', 'timeout', 'cancelled')",
		args...)
	if err != nil {
		return nil, prev, fmt.Errorf("terminalize: status: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, prev, fmt.Errorf("terminalize: status: %w", err)
	} else if n == 0 {
		return nil, prev, errTerminalCASMissed
	}

	rec, err := readTerminal(ctx, q, id)
	if err != nil {
		return nil, prev, err
	}
	return rec, prev, nil
}

const terminalCols = `dispatch_id, run_id, attempt, parent_dispatch_id, status, from_status, terminal_source,
	exit_code, exit_signal, failure_class, evidence_status, usage_status,
	input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
	producer_backend, producer_model, route_decision_id, report_path, report_sha256, artifacts_json,
	base_commit, base_worktree_digest, head_commit, head_worktree_digest,
	orphans_killed, contested, event_id, created_at`

// readTerminal returns the terminal record of dispatch id, or nil if it has none.
func readTerminal(ctx context.Context, q querier, id string) (*TerminalRecord, error) {
	var (
		r                                              TerminalRecord
		exitCode                                       sql.NullInt64
		exitSignal, failureClass                       sql.NullString
		inTok, outTok, cacheRead, cacheWrite           sql.NullInt64
		backend, model, reportPath, reportSHA          sql.NullString
		routeDecision                                  sql.NullInt64
		baseCommit, baseDigest, headCommit, headDigest sql.NullString
	)
	err := q.QueryRowContext(ctx, "SELECT "+terminalCols+" FROM dispatch_terminals WHERE dispatch_id = ?", id).Scan(
		&r.DispatchID, &r.RunID, &r.Attempt, &r.ParentDispatchID, &r.Status, &r.FromStatus, &r.Source,
		&exitCode, &exitSignal, &failureClass, &r.Evidence, &r.UsageStatus,
		&inTok, &outTok, &cacheRead, &cacheWrite,
		&backend, &model, &routeDecision, &reportPath, &reportSHA, &r.ArtifactsJSON,
		&baseCommit, &baseDigest, &headCommit, &headDigest,
		&r.OrphansKilled, &r.Contested, &r.EventID, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read terminal record: %w", err)
	}
	if exitCode.Valid {
		v := int(exitCode.Int64)
		r.ExitCode = &v
	}
	r.ExitSignal, r.FailureClass = exitSignal.String, failureClass.String
	r.InputTokens, r.OutputTokens = nullInt64Ptr(inTok), nullInt64Ptr(outTok)
	r.CacheReadTokens, r.CacheWriteTokens = nullInt64Ptr(cacheRead), nullInt64Ptr(cacheWrite)
	r.ProducerBackend, r.ProducerModel = backend.String, model.String
	r.RouteDecisionID = nullInt64Ptr(routeDecision)
	r.ReportPath, r.ReportSHA256 = reportPath.String, reportSHA.String
	r.BaseCommit, r.BaseWorktreeDigest = baseCommit.String, baseDigest.String
	r.HeadCommit, r.HeadWorktreeDigest = headCommit.String, headDigest.String
	return &r, nil
}

// withImmediateTx runs fn in a BEGIN IMMEDIATE transaction on a held
// connection and commits when fn returns nil. The connection is back in the
// pool when withImmediateTx returns. When ctx is done, the error it returns
// wraps ctx.Err() whichever statement the cancellation reached.
func withImmediateTx(ctx context.Context, db *sql.DB, op string, fn func(*sql.Conn) error) (err error) {
	defer func() {
		if cause := ctx.Err(); err != nil && cause != nil && !errors.Is(err, cause) {
			err = fmt.Errorf("%w: %w", cause, err)
		}
	}()
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("%s: acquire connection: %w", op, err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		return fmt.Errorf("%s: begin: %w", op, err)
	}
	committed := false
	defer func() {
		if !committed {
			// The request may already be cancelled; never return a connection with
			// an open transaction to the pool if cleanup itself fails.
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := conn.ExecContext(cleanup, "ROLLBACK"); err != nil {
				_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			}
		}
	}()
	if err := fn(conn); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("%s: commit: %w", op, err)
	}
	committed = true
	return nil
}

// traceContext is the propagated trace identity, read before the transaction.
type traceContext struct {
	traceID, spanID, parentSpanID string
}

func traceFromEnv() traceContext {
	return traceContext{
		traceID:      os.Getenv("IC_TRACE_ID"),
		spanID:       os.Getenv("IC_SPAN_ID"),
		parentSpanID: os.Getenv("IC_PARENT_SPAN_ID"),
	}
}

// terminalEnvelope has the JSON shape of event.EventEnvelope. The dispatch
// package cannot import event, which imports phase, which imports dispatch.
type terminalEnvelope struct {
	PolicyVersion      string   `json:"policy_version,omitempty"`
	CallerIdentity     string   `json:"caller_identity,omitempty"`
	CapabilityScope    string   `json:"capability_scope,omitempty"`
	TraceID            string   `json:"trace_id,omitempty"`
	SpanID             string   `json:"span_id,omitempty"`
	ParentSpanID       string   `json:"parent_span_id,omitempty"`
	InputArtifactRefs  []string `json:"input_artifact_refs,omitempty"`
	OutputArtifactRefs []string `json:"output_artifact_refs,omitempty"`
	RequestedSandbox   string   `json:"requested_sandbox,omitempty"`
	EffectiveSandbox   string   `json:"effective_sandbox,omitempty"`
}

// envelope builds what event.Store's default dispatch envelope would for the
// same transition.
func (tc traceContext) envelope(id, runID, from, to, requested, effective string) terminalEnvelope {
	traceID := tc.traceID
	if traceID == "" {
		traceID = runID
	}
	if traceID == "" {
		traceID = id
	}
	scope := "dispatch:" + id
	if runID != "" {
		scope = "run:" + runID
	}
	spanID := tc.spanID
	if spanID == "" {
		spanID = fmt.Sprintf("dispatch:%s:%d", id, time.Now().UnixNano())
	}
	return terminalEnvelope{
		PolicyVersion:      "dispatch-lifecycle/v2",
		CallerIdentity:     "dispatch.store",
		CapabilityScope:    scope,
		TraceID:            traceID,
		SpanID:             spanID,
		ParentSpanID:       tc.parentSpanID,
		InputArtifactRefs:  []string{from},
		OutputArtifactRefs: []string{to},
		RequestedSandbox:   requested,
		EffectiveSandbox:   effective,
	}
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt64Ptr(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	return &v.Int64
}
