package dispatch

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
)

const processIdentityVersion = 1

// processIdentity binds a host-created process to its OS birth, not merely a
// recyclable PID. Existing state storage avoids a new dispatch schema/database.
type processIdentity struct {
	Version int    `json:"version"`
	PID     int    `json:"pid"`
	GroupID int    `json:"group_id"`
	Birth   string `json:"birth"`
}

// Refuse before starting any child when this host cannot support governed
// cancellation. In particular, Darwin group enumeration must not silently
// degrade to a leader-only witness when kern.proc.all is denied.
func checkProcessInspection() error {
	identity, err := readProcessIdentity(os.Getpid())
	if err == nil {
		_, err = processGroupMembers(identity.GroupID)
	}
	if err != nil {
		return fmt.Errorf("host process inspection: %v: %w", err, &SpawnRejection{Reason: "process_identity_unavailable"})
	}
	return nil
}

func (s *Store) recordProcessIdentity(ctx context.Context, id string, pid int) (processIdentity, error) {
	identity, err := readProcessIdentity(pid)
	if err != nil {
		return identity, fmt.Errorf("read child identity: %v: %w", err, &SpawnRejection{Reason: "process_identity_unavailable"})
	}
	if identity.PID != pid || identity.GroupID != pid || identity.Birth == "" {
		return identity, &SpawnRejection{Reason: "process_identity_unavailable"}
	}
	payload, err := json.Marshal(identity)
	if err != nil {
		return identity, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return identity, err
	}
	defer tx.Rollback()
	// The first SQL statement is a write. It acquires SQLite's writer slot before
	// any read, avoiding a deferred read-to-write upgrade racing other admissions.
	_, err = tx.ExecContext(ctx, `INSERT INTO state(key,scope_id,payload,updated_at) VALUES ('dispatch.process',?,?,unixepoch())`, id, string(payload))
	if err != nil {
		return identity, err
	}
	var scope sql.NullString
	result, err := tx.ExecContext(ctx, `UPDATE dispatches SET status='running',pid=?,started_at=unixepoch() WHERE id=? AND status='spawned'`, pid, id)
	if err != nil {
		return identity, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return identity, err
	}
	if n != 1 {
		return identity, ErrStaleStatus
	}
	if err := tx.QueryRowContext(ctx, `SELECT scope_id FROM dispatches WHERE id=?`, id).Scan(&scope); err != nil {
		return identity, err
	}
	if err := tx.Commit(); err != nil {
		return identity, err
	}
	if s.eventRecorder != nil {
		s.eventRecorder(id, scope.String, StatusSpawned, StatusRunning)
	}
	return identity, nil
}

func (s *Store) processIdentity(ctx context.Context, d *Dispatch) (processIdentity, error) {
	var identity processIdentity
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT payload FROM state WHERE key='dispatch.process' AND scope_id=? AND expires_at IS NULL`, d.ID).Scan(&raw)
	if err != nil {
		return identity, err
	}
	if err := json.Unmarshal([]byte(raw), &identity); err != nil {
		return identity, err
	}
	if d.PID == nil || identity.PID != *d.PID || identity.PID <= 1 || identity.GroupID != identity.PID || identity.Birth == "" || identity.Version != processIdentityVersion {
		return identity, fmt.Errorf("invalid recorded process identity")
	}
	return identity, nil
}

// A successfully observed different birth proves the recorded process is gone.
// A read failure, legacy format, or changed group does not. Versioning prevents
// migration from Darwin's old clock-derived birth from looking like PID reuse.
func processIdentityAlive(expected processIdentity) (bool, error) {
	if expected.Version != processIdentityVersion || expected.Birth == "" {
		return false, fmt.Errorf("unsupported recorded process identity")
	}
	if !isProcessAlive(expected.PID) {
		return false, nil
	}
	actual, err := readProcessIdentity(expected.PID)
	if err != nil {
		if !isProcessAlive(expected.PID) {
			return false, nil
		}
		return false, err
	}
	if actual.Version != expected.Version || actual.PID != expected.PID {
		return false, fmt.Errorf("process identity format unavailable")
	}
	if actual.Birth != expected.Birth {
		return false, nil
	}
	if actual.GroupID != expected.GroupID {
		return false, fmt.Errorf("process group changed")
	}
	return true, nil
}

func matchesProcessIdentity(expected processIdentity) bool {
	actual, err := readProcessIdentity(expected.PID)
	return err == nil && expected.Birth != "" && actual == expected
}
