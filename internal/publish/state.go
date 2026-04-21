package publish

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
)

const publishStateSchema = `
CREATE TABLE IF NOT EXISTS publish_state (
    id         TEXT PRIMARY KEY,
    plugin     TEXT NOT NULL,
    from_ver   TEXT NOT NULL,
    to_ver     TEXT NOT NULL,
    phase      TEXT NOT NULL DEFAULT 'idle',
    root       TEXT NOT NULL,
    market     TEXT NOT NULL,
    started_at INTEGER NOT NULL DEFAULT (unixepoch()),
    updated_at INTEGER NOT NULL DEFAULT (unixepoch()),
    error      TEXT NOT NULL DEFAULT ''
);`

// Store provides publish state operations against the intercore DB.
type Store struct {
	db *sql.DB
}

// NewStore creates a publish state store.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// DB returns the underlying database handle. Used by callers that need to
// share the same connection pool (e.g., the v2 token-consume path in
// RequiresApproval, which must use the same *sql.DB to serialize under
// MaxOpenConns=1).
func (s *Store) DB() *sql.DB {
	return s.db
}

// EnsureTable creates the publish_state table if it doesn't exist.
func (s *Store) EnsureTable(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, publishStateSchema)
	return err
}

// Create inserts a new publish state record.
func (s *Store) Create(ctx context.Context, st *PublishState) error {
	id, err := generatePublishID()
	if err != nil {
		return fmt.Errorf("generate ID: %w", err)
	}
	st.ID = id

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO publish_state (id, plugin, from_ver, to_ver, phase, root, market)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		st.ID, st.PluginName, st.FromVersion, st.ToVersion,
		string(st.Phase), st.PluginRoot, st.MarketRoot)
	if err != nil {
		return fmt.Errorf("insert publish_state: %w", err)
	}
	return nil
}

// Update sets the current phase and error for a publish state.
func (s *Store) Update(ctx context.Context, id string, phase Phase, errMsg string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE publish_state
		SET phase = ?, error = ?, updated_at = unixepoch()
		WHERE id = ?`,
		string(phase), errMsg, id)
	if err != nil {
		return fmt.Errorf("update publish_state: %w", err)
	}
	return nil
}

// Get retrieves a publish state by ID.
func (s *Store) Get(ctx context.Context, id string) (*PublishState, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, plugin, from_ver, to_ver, phase, root, market, started_at, updated_at, error
		FROM publish_state WHERE id = ?`, id)

	var st PublishState
	var phase string
	if err := row.Scan(&st.ID, &st.PluginName, &st.FromVersion, &st.ToVersion,
		&phase, &st.PluginRoot, &st.MarketRoot, &st.StartedAt, &st.UpdatedAt, &st.Error); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNoActivePublish
		}
		return nil, fmt.Errorf("get publish_state: %w", err)
	}
	st.Phase = Phase(phase)
	return &st, nil
}

// GetActive returns the most recent incomplete publish for a plugin, if any.
func (s *Store) GetActive(ctx context.Context, pluginName string) (*PublishState, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, plugin, from_ver, to_ver, phase, root, market, started_at, updated_at, error
		FROM publish_state
		WHERE plugin = ? AND phase != 'done' AND phase != 'idle'
		ORDER BY started_at DESC LIMIT 1`, pluginName)

	var st PublishState
	var phase string
	if err := row.Scan(&st.ID, &st.PluginName, &st.FromVersion, &st.ToVersion,
		&phase, &st.PluginRoot, &st.MarketRoot, &st.StartedAt, &st.UpdatedAt, &st.Error); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil // no active publish
		}
		return nil, fmt.Errorf("get active: %w", err)
	}
	st.Phase = Phase(phase)
	return &st, nil
}

// Complete marks a publish as done and clears the error.
func (s *Store) Complete(ctx context.Context, id string) error {
	return s.Update(ctx, id, PhaseDone, "")
}

// Delete removes a publish state record.
func (s *Store) Delete(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM publish_state WHERE id = ?", id)
	return err
}

// List returns all publish state records.
func (s *Store) List(ctx context.Context) ([]*PublishState, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, plugin, from_ver, to_ver, phase, root, market, started_at, updated_at, error
		FROM publish_state ORDER BY started_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list publish_state: %w", err)
	}
	defer rows.Close()

	var states []*PublishState
	for rows.Next() {
		var st PublishState
		var phase string
		if err := rows.Scan(&st.ID, &st.PluginName, &st.FromVersion, &st.ToVersion,
			&phase, &st.PluginRoot, &st.MarketRoot, &st.StartedAt, &st.UpdatedAt, &st.Error); err != nil {
			return nil, fmt.Errorf("scan publish_state: %w", err)
		}
		st.Phase = Phase(phase)
		states = append(states, &st)
	}
	return states, rows.Err()
}

func generatePublishID() (string, error) {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	const idLen = 8
	b := make([]byte, idLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = chars[int(b[i])%len(chars)]
	}
	return "pub-" + string(b), nil
}
