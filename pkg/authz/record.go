package authz

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// RecordArgs stores one authorization decision in the audit table.
type RecordArgs struct {
	ID             string
	OpType         string
	Target         string
	AgentID        string
	BeadID         string
	Mode           string
	PolicyMatch    string
	PolicyHash     string
	VettedSHA      string
	Vetting        map[string]interface{}
	CrossProjectID string
	CreatedAt      int64
}

// Record inserts one row into authorizations.
func Record(db *sql.DB, args RecordArgs) error {
	if db == nil {
		return fmt.Errorf("nil db")
	}
	if args.OpType == "" {
		return fmt.Errorf("op_type is required")
	}
	if args.Target == "" {
		return fmt.Errorf("target is required")
	}
	if args.AgentID == "" {
		return fmt.Errorf("agent_id is required")
	}
	if !isRecordMode(args.Mode) {
		return fmt.Errorf("invalid mode %q", args.Mode)
	}

	if args.ID == "" {
		id, err := randomID()
		if err != nil {
			return fmt.Errorf("generate id: %w", err)
		}
		args.ID = id
	}
	if args.CreatedAt == 0 {
		args.CreatedAt = time.Now().Unix()
	}

	var vettingJSON interface{}
	if args.Vetting != nil {
		data, err := json.Marshal(args.Vetting)
		if err != nil {
			return fmt.Errorf("marshal vetting: %w", err)
		}
		vettingJSON = string(data)
	}

	// sig_version=1 marks the row as signable under the current (v1.5)
	// signing scheme. Pre-migration rows keep sig_version=0 (via schema
	// default) — "pre-signing vintage". The column must be set explicitly
	// here so new post-cutover rows don't inherit the 0 default.
	_, err := db.Exec(`
		INSERT INTO authorizations (
			id, op_type, target, agent_id, bead_id, mode,
			policy_match, policy_hash, vetted_sha, vetting,
			cross_project_id, created_at, sig_version
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1)
	`,
		args.ID,
		args.OpType,
		args.Target,
		args.AgentID,
		nullIfEmpty(args.BeadID),
		args.Mode,
		nullIfEmpty(args.PolicyMatch),
		nullIfEmpty(args.PolicyHash),
		nullIfEmpty(args.VettedSHA),
		vettingJSON,
		nullIfEmpty(args.CrossProjectID),
		args.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert authorization record: %w", err)
	}
	return nil
}

func isRecordMode(mode string) bool {
	switch mode {
	case "auto", "confirmed", "blocked", "force_auto":
		return true
	default:
		return false
	}
}

func randomID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func nullIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}
