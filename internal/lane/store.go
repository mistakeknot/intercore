package lane

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"time"
)

const idChars = "abcdefghijklmnopqrstuvwxyz0123456789"
const idLen = 8

// Lane represents a thematic work lane.
type Lane struct {
	ID          string
	Name        string
	LaneType    string // "standing" or "arc"
	Status      string // "active", "closed", "archived"
	Description string
	Metadata    string // JSON
	CreatedAt   int64
	UpdatedAt   int64
	ClosedAt    *int64
}

// LaneEvent represents an event in a lane's history.
type LaneEvent struct {
	ID        int64
	LaneID    string
	EventType string
	Payload   string
	CreatedAt int64
}

// Store provides lane CRUD operations against the intercore DB.
type Store struct {
	db *sql.DB
}

// New creates a lane store.
func New(db *sql.DB) *Store {
	return &Store{db: db}
}

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

// Create inserts a new lane and records a "created" event.
func (s *Store) Create(ctx context.Context, name, laneType, description string) (string, error) {
	id, err := generateID()
	if err != nil {
		return "", err
	}

	now := time.Now().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("lane create: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO lanes (id, name, lane_type, description, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		id, name, laneType, description, now, now,
	)
	if err != nil {
		return "", fmt.Errorf("lane create: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO lane_events (lane_id, event_type, payload, created_at)
		VALUES (?, 'created', '{}', ?)`,
		id, now,
	)
	if err != nil {
		return "", fmt.Errorf("lane create event: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("lane create: commit: %w", err)
	}
	return id, nil
}

// Get retrieves a lane by ID.
func (s *Store) Get(ctx context.Context, id string) (*Lane, error) {
	l := &Lane{}
	err := s.db.QueryRowContext(ctx, `
		SELECT id, name, lane_type, status, description, metadata, created_at, updated_at, closed_at
		FROM lanes WHERE id = ?`, id,
	).Scan(&l.ID, &l.Name, &l.LaneType, &l.Status, &l.Description, &l.Metadata,
		&l.CreatedAt, &l.UpdatedAt, &l.ClosedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("lane not found: %s", id)
	}
	if err != nil {
		return nil, fmt.Errorf("lane get: %w", err)
	}
	return l, nil
}

// GetByName retrieves a lane by name.
func (s *Store) GetByName(ctx context.Context, name string) (*Lane, error) {
	l := &Lane{}
	err := s.db.QueryRowContext(ctx, `
		SELECT id, name, lane_type, status, description, metadata, created_at, updated_at, closed_at
		FROM lanes WHERE name = ?`, name,
	).Scan(&l.ID, &l.Name, &l.LaneType, &l.Status, &l.Description, &l.Metadata,
		&l.CreatedAt, &l.UpdatedAt, &l.ClosedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("lane not found: %s", name)
	}
	if err != nil {
		return nil, fmt.Errorf("lane get by name: %w", err)
	}
	return l, nil
}

// List returns lanes filtered by status. Empty status returns all.
func (s *Store) List(ctx context.Context, status string) ([]*Lane, error) {
	var rows *sql.Rows
	var err error
	if status != "" {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, name, lane_type, status, description, metadata, created_at, updated_at, closed_at
			FROM lanes WHERE status = ? ORDER BY name`, status)
	} else {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, name, lane_type, status, description, metadata, created_at, updated_at, closed_at
			FROM lanes ORDER BY name`)
	}
	if err != nil {
		return nil, fmt.Errorf("lane list: %w", err)
	}
	defer rows.Close()

	var lanes []*Lane
	for rows.Next() {
		l := &Lane{}
		if err := rows.Scan(&l.ID, &l.Name, &l.LaneType, &l.Status, &l.Description,
			&l.Metadata, &l.CreatedAt, &l.UpdatedAt, &l.ClosedAt); err != nil {
			return nil, fmt.Errorf("lane list scan: %w", err)
		}
		lanes = append(lanes, l)
	}
	return lanes, rows.Err()
}

// Close marks a lane as closed and records a "closed" event.
func (s *Store) Close(ctx context.Context, id string) error {
	now := time.Now().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("lane close: begin: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		UPDATE lanes SET status = 'closed', closed_at = ?, updated_at = ?
		WHERE id = ? AND status = 'active'`, now, now, id)
	if err != nil {
		return fmt.Errorf("lane close: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("lane not found or already closed: %s", id)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO lane_events (lane_id, event_type, payload, created_at)
		VALUES (?, 'closed', '{}', ?)`, id, now)
	if err != nil {
		return fmt.Errorf("lane close event: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("lane close: commit: %w", err)
	}
	return nil
}

// RecordEvent inserts a lane event.
func (s *Store) RecordEvent(ctx context.Context, laneID, eventType, payload string) error {
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO lane_events (lane_id, event_type, payload, created_at)
		VALUES (?, ?, ?, ?)`, laneID, eventType, payload, now)
	if err != nil {
		return fmt.Errorf("lane record event: %w", err)
	}
	return nil
}

// Events returns events for a lane, ordered by creation time.
func (s *Store) Events(ctx context.Context, laneID string) ([]*LaneEvent, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, lane_id, event_type, payload, created_at
		FROM lane_events WHERE lane_id = ? ORDER BY created_at`, laneID)
	if err != nil {
		return nil, fmt.Errorf("lane events: %w", err)
	}
	defer rows.Close()

	var events []*LaneEvent
	for rows.Next() {
		e := &LaneEvent{}
		if err := rows.Scan(&e.ID, &e.LaneID, &e.EventType, &e.Payload, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("lane events scan: %w", err)
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

// SnapshotMembers replaces the current member set for a lane.
// Adds new members, removes stale ones, records events.
func (s *Store) SnapshotMembers(ctx context.Context, laneID string, beadIDs []string) error {
	now := time.Now().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("snapshot members: begin: %w", err)
	}
	defer tx.Rollback()

	// Get current members
	existing := make(map[string]bool)
	rows, err := tx.QueryContext(ctx, `SELECT bead_id FROM lane_members WHERE lane_id = ?`, laneID)
	if err != nil {
		return fmt.Errorf("snapshot members: query: %w", err)
	}
	for rows.Next() {
		var bid string
		if err := rows.Scan(&bid); err != nil {
			rows.Close()
			return fmt.Errorf("snapshot members: scan: %w", err)
		}
		existing[bid] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("snapshot members: rows: %w", err)
	}

	// Build desired set
	desired := make(map[string]bool, len(beadIDs))
	for _, bid := range beadIDs {
		desired[bid] = true
	}

	// Add new members
	for _, bid := range beadIDs {
		if !existing[bid] {
			_, err := tx.ExecContext(ctx, `
				INSERT INTO lane_members (lane_id, bead_id, added_at)
				VALUES (?, ?, ?)`, laneID, bid, now)
			if err != nil {
				return fmt.Errorf("snapshot members: insert %s: %w", bid, err)
			}
		}
	}

	// Remove stale members
	for bid := range existing {
		if !desired[bid] {
			_, err := tx.ExecContext(ctx, `
				DELETE FROM lane_members WHERE lane_id = ? AND bead_id = ?`, laneID, bid)
			if err != nil {
				return fmt.Errorf("snapshot members: delete %s: %w", bid, err)
			}
		}
	}

	// Record snapshot event
	_, err = tx.ExecContext(ctx, `
		INSERT INTO lane_events (lane_id, event_type, payload, created_at)
		VALUES (?, 'snapshot', ?, ?)`,
		laneID, fmt.Sprintf(`{"count":%d}`, len(beadIDs)), now)
	if err != nil {
		return fmt.Errorf("snapshot members: event: %w", err)
	}

	// Update lane timestamp
	_, err = tx.ExecContext(ctx, `UPDATE lanes SET updated_at = ? WHERE id = ?`, now, laneID)
	if err != nil {
		return fmt.Errorf("snapshot members: update lane: %w", err)
	}

	return tx.Commit()
}

// GetMembers returns bead IDs in a lane.
func (s *Store) GetMembers(ctx context.Context, laneID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT bead_id FROM lane_members WHERE lane_id = ? ORDER BY added_at`, laneID)
	if err != nil {
		return nil, fmt.Errorf("get members: %w", err)
	}
	defer rows.Close()

	var members []string
	for rows.Next() {
		var bid string
		if err := rows.Scan(&bid); err != nil {
			return nil, fmt.Errorf("get members scan: %w", err)
		}
		members = append(members, bid)
	}
	return members, rows.Err()
}

// GetLanesForBead returns lane IDs that contain the given bead.
func (s *Store) GetLanesForBead(ctx context.Context, beadID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT lane_id FROM lane_members WHERE bead_id = ?`, beadID)
	if err != nil {
		return nil, fmt.Errorf("get lanes for bead: %w", err)
	}
	defer rows.Close()

	var lanes []string
	for rows.Next() {
		var lid string
		if err := rows.Scan(&lid); err != nil {
			return nil, fmt.Errorf("get lanes for bead scan: %w", err)
		}
		lanes = append(lanes, lid)
	}
	return lanes, rows.Err()
}
