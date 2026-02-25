// Package audit provides tamper-evident audit logging backed by SQLite.
//
// Each entry is part of a SHA-256 hash chain: every entry includes the
// checksum of the previous entry, enabling integrity verification.
// Sequence numbers per session detect gaps from deletion.
//
// All string values in payload/metadata are automatically redacted before
// persistence using the redaction package.
//
// Inspired by github.com/Dicklesworthstone/ntm/internal/audit, adapted
// for SQLite storage and Intercore integration.
package audit

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/mistakeknot/intercore/internal/redaction"
)

// EventType represents the type of audit event.
type EventType string

const (
	EventCommand     EventType = "command"
	EventSpawn       EventType = "spawn"
	EventSend        EventType = "send"
	EventReserve     EventType = "reserve"
	EventRelease     EventType = "release"
	EventTransfer    EventType = "transfer"
	EventApprove     EventType = "approve"
	EventDeny        EventType = "deny"
	EventStateChange EventType = "state_change"
	EventError       EventType = "error"
	EventHandoff     EventType = "handoff"
)

// Actor represents who performed the action.
type Actor string

const (
	ActorUser   Actor = "user"
	ActorAgent  Actor = "agent"
	ActorSystem Actor = "system"
)

// Entry represents a single audit log entry.
type Entry struct {
	ID          int64                  `json:"id,omitempty"`
	SessionID   string                 `json:"session_id"`
	EventType   EventType              `json:"event_type"`
	Actor       Actor                  `json:"actor"`
	Target      string                 `json:"target"`
	Payload     map[string]interface{} `json:"payload"`
	Metadata    map[string]interface{} `json:"metadata"`
	PrevHash    string                 `json:"prev_hash,omitempty"`
	Checksum    string                 `json:"checksum"`
	SequenceNum uint64                 `json:"sequence_num"`
	CreatedAt   time.Time              `json:"created_at"`
}

// Logger provides tamper-evident audit logging to SQLite.
type Logger struct {
	db          *sql.DB
	sessionID   string
	mu          sync.Mutex
	lastHash    string
	sequenceNum uint64
	redactCfg   redaction.Config
}

// New creates a new audit logger for the given session.
func New(db *sql.DB, sessionID string) (*Logger, error) {
	l := &Logger{
		db:        db,
		sessionID: sessionID,
		redactCfg: redaction.Config{Mode: redaction.ModeRedact},
	}

	// Load last hash and sequence number from existing entries.
	if err := l.loadLastEntry(); err != nil {
		return nil, fmt.Errorf("audit.New: %w", err)
	}

	return l, nil
}

// SetRedactionConfig sets the redaction config for payload sanitization.
func (l *Logger) SetRedactionConfig(cfg redaction.Config) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.redactCfg = cfg
}

func (l *Logger) loadLastEntry() error {
	var prevHash sql.NullString
	var seqNum sql.NullInt64

	err := l.db.QueryRow(
		`SELECT checksum, sequence_num FROM audit_log
		 WHERE session_id = ? ORDER BY sequence_num DESC LIMIT 1`,
		l.sessionID,
	).Scan(&prevHash, &seqNum)

	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load last entry: %w", err)
	}

	if prevHash.Valid {
		l.lastHash = prevHash.String
	}
	if seqNum.Valid {
		l.sequenceNum = uint64(seqNum.Int64)
	}
	return nil
}

// Log writes an audit entry to the database.
func (l *Logger) Log(ctx context.Context, eventType EventType, actor Actor, target string, payload, metadata map[string]interface{}) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	// Truncate to Unix seconds for consistency with SQLite storage.
	// This ensures checksum verification works after read-back.
	now := time.Unix(time.Now().Unix(), 0).UTC()
	l.sequenceNum++

	entry := Entry{
		SessionID:   l.sessionID,
		EventType:   eventType,
		Actor:       actor,
		Target:      target,
		Payload:     l.sanitizeMap(payload),
		Metadata:    l.sanitizeMap(metadata),
		PrevHash:    l.lastHash,
		SequenceNum: l.sequenceNum,
		CreatedAt:   now,
	}

	// Compute checksum over the entry (without the checksum field).
	checksum, err := computeChecksum(entry)
	if err != nil {
		return fmt.Errorf("audit.Log: compute checksum: %w", err)
	}
	entry.Checksum = checksum

	// Marshal payload and metadata to JSON.
	payloadJSON, err := json.Marshal(entry.Payload)
	if err != nil {
		return fmt.Errorf("audit.Log: marshal payload: %w", err)
	}
	metadataJSON, err := json.Marshal(entry.Metadata)
	if err != nil {
		return fmt.Errorf("audit.Log: marshal metadata: %w", err)
	}

	_, err = l.db.ExecContext(ctx,
		`INSERT INTO audit_log (session_id, event_type, actor, target, payload, metadata, prev_hash, checksum, sequence_num, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		entry.SessionID,
		string(entry.EventType),
		string(entry.Actor),
		entry.Target,
		string(payloadJSON),
		string(metadataJSON),
		entry.PrevHash,
		entry.Checksum,
		entry.SequenceNum,
		now.Unix(),
	)
	if err != nil {
		return fmt.Errorf("audit.Log: insert: %w", err)
	}

	l.lastHash = entry.Checksum
	return nil
}

func computeChecksum(entry Entry) (string, error) {
	// Zero out checksum for hashing.
	e := entry
	e.Checksum = ""
	e.ID = 0

	data, err := json.Marshal(e)
	if err != nil {
		return "", err
	}

	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:]), nil
}

// VerifyIntegrity verifies the hash chain for a session.
// Returns nil if the chain is valid, or an error describing the first break.
func VerifyIntegrity(ctx context.Context, db *sql.DB, sessionID string) error {
	rows, err := db.QueryContext(ctx,
		`SELECT session_id, event_type, actor, target, payload, metadata, prev_hash, checksum, sequence_num, created_at
		 FROM audit_log WHERE session_id = ? ORDER BY sequence_num ASC`,
		sessionID,
	)
	if err != nil {
		return fmt.Errorf("verify: query: %w", err)
	}
	defer rows.Close()

	var prevHash string
	var expectedSeq uint64

	for rows.Next() {
		var (
			sid, et, act, tgt string
			payloadJSON, metadataJSON string
			ph, cs string
			sn     int64
			ca     int64
		)
		if err := rows.Scan(&sid, &et, &act, &tgt, &payloadJSON, &metadataJSON, &ph, &cs, &sn, &ca); err != nil {
			return fmt.Errorf("verify: scan: %w", err)
		}

		expectedSeq++
		if uint64(sn) != expectedSeq {
			return fmt.Errorf("sequence gap at %d: expected %d", sn, expectedSeq)
		}

		if ph != prevHash {
			return fmt.Errorf("hash chain broken at sequence %d: prev_hash mismatch", sn)
		}

		// Reconstruct entry for checksum verification.
		var payload, metadata map[string]interface{}
		json.Unmarshal([]byte(payloadJSON), &payload)
		json.Unmarshal([]byte(metadataJSON), &metadata)

		entry := Entry{
			SessionID:   sid,
			EventType:   EventType(et),
			Actor:       Actor(act),
			Target:      tgt,
			Payload:     payload,
			Metadata:    metadata,
			PrevHash:    ph,
			SequenceNum: uint64(sn),
			CreatedAt:   time.Unix(ca, 0).UTC(),
		}

		computed, err := computeChecksum(entry)
		if err != nil {
			return fmt.Errorf("verify: compute checksum at seq %d: %w", sn, err)
		}

		if computed != cs {
			return fmt.Errorf("checksum mismatch at sequence %d", sn)
		}

		prevHash = cs
	}

	return rows.Err()
}

// Query returns audit entries matching the filter.
func Query(ctx context.Context, db *sql.DB, filter Filter) ([]Entry, error) {
	query := `SELECT id, session_id, event_type, actor, target, payload, metadata, prev_hash, checksum, sequence_num, created_at
	          FROM audit_log WHERE 1=1`
	var args []interface{}

	if filter.SessionID != "" {
		query += " AND session_id = ?"
		args = append(args, filter.SessionID)
	}
	if filter.EventType != "" {
		query += " AND event_type = ?"
		args = append(args, string(filter.EventType))
	}
	if filter.Actor != "" {
		query += " AND actor = ?"
		args = append(args, string(filter.Actor))
	}
	if !filter.Since.IsZero() {
		query += " AND created_at >= ?"
		args = append(args, filter.Since.Unix())
	}
	if !filter.Until.IsZero() {
		query += " AND created_at <= ?"
		args = append(args, filter.Until.Unix())
	}

	query += " ORDER BY created_at ASC"

	if filter.Limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", filter.Limit)
	}

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("audit.Query: %w", err)
	}
	defer rows.Close()

	var entries []Entry
	for rows.Next() {
		var e Entry
		var payloadJSON, metadataJSON string
		var ca int64

		if err := rows.Scan(&e.ID, &e.SessionID, &e.EventType, &e.Actor, &e.Target,
			&payloadJSON, &metadataJSON, &e.PrevHash, &e.Checksum, &e.SequenceNum, &ca); err != nil {
			return nil, fmt.Errorf("audit.Query: scan: %w", err)
		}
		json.Unmarshal([]byte(payloadJSON), &e.Payload)
		json.Unmarshal([]byte(metadataJSON), &e.Metadata)
		e.CreatedAt = time.Unix(ca, 0).UTC()
		entries = append(entries, e)
	}

	return entries, rows.Err()
}

// Filter specifies criteria for querying audit entries.
type Filter struct {
	SessionID string
	EventType EventType
	Actor     Actor
	Since     time.Time
	Until     time.Time
	Limit     int
}

// sanitizeMap redacts sensitive values in a map.
func (l *Logger) sanitizeMap(m map[string]interface{}) map[string]interface{} {
	if m == nil {
		return nil
	}
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		out[k] = l.sanitizeValue(v)
	}
	return out
}

func (l *Logger) sanitizeValue(v interface{}) interface{} {
	switch val := v.(type) {
	case string:
		if l.redactCfg.Mode == redaction.ModeOff {
			return val
		}
		result := redaction.ScanAndRedact(val, l.redactCfg)
		return result.Output
	case []string:
		out := make([]string, len(val))
		for i, s := range val {
			out[i] = l.sanitizeValue(s).(string)
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(val))
		for i, item := range val {
			out[i] = l.sanitizeValue(item)
		}
		return out
	case map[string]interface{}:
		return l.sanitizeMap(val)
	default:
		return v
	}
}
