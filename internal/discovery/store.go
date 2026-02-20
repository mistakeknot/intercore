package discovery

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Store provides discovery CRUD and event operations.
type Store struct {
	db *sql.DB
}

// NewStore creates a discovery store.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// Submit creates a new discovery record and emits a discovery.submitted event.
// Returns the generated ID.
func (s *Store) Submit(ctx context.Context, source, sourceID, title, summary, url, rawMetadata string, embedding []byte, score float64) (string, error) {
	id, err := generateID()
	if err != nil {
		return "", fmt.Errorf("submit: %w", err)
	}

	tier := TierFromScore(score)
	now := nowUnix()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("submit: begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO discoveries (id, source, source_id, title, summary, url, raw_metadata, embedding, relevance_score, confidence_tier, status, discovered_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, source, sourceID, title, summary, url, rawMetadata, embedding, score, tier, StatusNew, now,
	)
	if err != nil {
		if isUniqueConstraintError(err) {
			return "", fmt.Errorf("%w: source=%s source_id=%s", ErrDuplicate, source, sourceID)
		}
		return "", fmt.Errorf("submit: insert: %w", err)
	}

	// Emit submitted event
	payload, _ := json.Marshal(map[string]interface{}{
		"id": id, "source": source, "title": title, "score": score, "tier": tier,
	})
	_, err = tx.ExecContext(ctx, `
		INSERT INTO discovery_events (discovery_id, event_type, from_status, to_status, payload, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		id, EventSubmitted, "", StatusNew, string(payload), now,
	)
	if err != nil {
		return "", fmt.Errorf("submit: event: %w", err)
	}

	return id, tx.Commit()
}

// SubmitWithDedup checks for embedding similarity duplicates before inserting.
// If a match above threshold is found, returns the existing ID and emits a dedup event.
// Scan + insert happen in a single transaction to prevent TOCTOU.
func (s *Store) SubmitWithDedup(ctx context.Context, source, sourceID, title, summary, url, rawMetadata string, embedding []byte, score float64, dedupThreshold float64) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("submit dedup: begin: %w", err)
	}
	defer tx.Rollback()

	// Scan existing embeddings for this source
	rows, err := tx.QueryContext(ctx,
		"SELECT id, embedding FROM discoveries WHERE source = ? AND embedding IS NOT NULL", source)
	if err != nil {
		return "", fmt.Errorf("submit dedup: scan: %w", err)
	}

	var existingID string
	var found bool
	for rows.Next() {
		var eid string
		var eemb []byte
		if err := rows.Scan(&eid, &eemb); err != nil {
			rows.Close()
			return "", fmt.Errorf("submit dedup: scan row: %w", err)
		}
		sim := CosineSimilarity(embedding, eemb)
		if sim >= dedupThreshold {
			existingID = eid
			found = true
			break
		}
	}
	rows.Close()

	if found {
		// Dedup hit — emit event and return existing ID
		now := nowUnix()
		payload, _ := json.Marshal(map[string]interface{}{
			"existing_id": existingID, "source": source, "new_source_id": sourceID,
		})
		_, _ = tx.ExecContext(ctx, `
			INSERT INTO discovery_events (discovery_id, event_type, payload, created_at)
			VALUES (?, ?, ?, ?)`,
			existingID, EventDeduped, string(payload), now)
		if err := tx.Commit(); err != nil {
			return "", fmt.Errorf("submit dedup: commit dedup event: %w", err)
		}
		return existingID, nil
	}

	// No dedup hit — insert new record within the same transaction
	id, err := generateID()
	if err != nil {
		return "", fmt.Errorf("submit dedup: %w", err)
	}
	tier := TierFromScore(score)
	now := nowUnix()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO discoveries (id, source, source_id, title, summary, url, raw_metadata, embedding, relevance_score, confidence_tier, status, discovered_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, source, sourceID, title, summary, url, rawMetadata, embedding, score, tier, StatusNew, now,
	)
	if err != nil {
		if isUniqueConstraintError(err) {
			return "", fmt.Errorf("%w: source=%s source_id=%s", ErrDuplicate, source, sourceID)
		}
		return "", fmt.Errorf("submit dedup: insert: %w", err)
	}

	payload, _ := json.Marshal(map[string]interface{}{
		"id": id, "source": source, "title": title, "score": score, "tier": tier,
	})
	_, err = tx.ExecContext(ctx, `
		INSERT INTO discovery_events (discovery_id, event_type, from_status, to_status, payload, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		id, EventSubmitted, "", StatusNew, string(payload), now,
	)
	if err != nil {
		return "", fmt.Errorf("submit dedup: event: %w", err)
	}

	return id, tx.Commit()
}

// Get returns a single discovery by ID.
func (s *Store) Get(ctx context.Context, id string) (*Discovery, error) {
	var d Discovery
	var runID, beadID sql.NullString
	var promotedAt, reviewedAt sql.NullInt64
	var embedding []byte

	err := s.db.QueryRowContext(ctx, `
		SELECT id, source, source_id, title, summary, url, raw_metadata, embedding,
			relevance_score, confidence_tier, status, run_id, bead_id,
			discovered_at, promoted_at, reviewed_at
		FROM discoveries WHERE id = ?`, id,
	).Scan(
		&d.ID, &d.Source, &d.SourceID, &d.Title, &d.Summary, &d.URL, &d.RawMetadata, &embedding,
		&d.RelevanceScore, &d.ConfidenceTier, &d.Status, &runID, &beadID,
		&d.DiscoveredAt, &promotedAt, &reviewedAt,
	)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("get: %w", err)
	}

	d.Embedding = embedding
	if runID.Valid {
		d.RunID = &runID.String
	}
	if beadID.Valid {
		d.BeadID = &beadID.String
	}
	if promotedAt.Valid {
		d.PromotedAt = &promotedAt.Int64
	}
	if reviewedAt.Valid {
		d.ReviewedAt = &reviewedAt.Int64
	}
	return &d, nil
}

// ListFilter controls what List returns.
type ListFilter struct {
	Source string
	Status string
	Tier   string
	Limit  int
}

// List returns discoveries matching the filter, sorted by relevance_score DESC, id ASC.
func (s *Store) List(ctx context.Context, f ListFilter) ([]Discovery, error) {
	if f.Limit <= 0 {
		f.Limit = 100
	}

	query := "SELECT id, source, source_id, title, summary, url, relevance_score, confidence_tier, status, discovered_at FROM discoveries WHERE 1=1"
	var args []interface{}

	if f.Source != "" {
		query += " AND source = ?"
		args = append(args, f.Source)
	}
	if f.Status != "" {
		query += " AND status = ?"
		args = append(args, f.Status)
	}
	if f.Tier != "" {
		query += " AND confidence_tier = ?"
		args = append(args, f.Tier)
	}
	query += " ORDER BY relevance_score DESC, id ASC LIMIT ?"
	args = append(args, f.Limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list: %w", err)
	}
	defer rows.Close()

	var results []Discovery
	for rows.Next() {
		var d Discovery
		if err := rows.Scan(&d.ID, &d.Source, &d.SourceID, &d.Title, &d.Summary, &d.URL,
			&d.RelevanceScore, &d.ConfidenceTier, &d.Status, &d.DiscoveredAt); err != nil {
			return nil, fmt.Errorf("list scan: %w", err)
		}
		results = append(results, d)
	}
	return results, rows.Err()
}

// Score updates a discovery's relevance score and recomputes tier.
// Returns ErrNotFound if missing, ErrLifecycle if dismissed/promoted.
func (s *Store) Score(ctx context.Context, id string, score float64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("score: begin: %w", err)
	}
	defer tx.Rollback()

	// Verify existence and check lifecycle status
	var status string
	var oldScore float64
	err = tx.QueryRowContext(ctx, "SELECT status, relevance_score FROM discoveries WHERE id = ?", id).Scan(&status, &oldScore)
	if err == sql.ErrNoRows {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return fmt.Errorf("score: lookup: %w", err)
	}

	// Prevent scoring dismissed or promoted discoveries
	if status == StatusDismissed || status == StatusPromoted {
		return fmt.Errorf("%w: cannot score discovery in %q status", ErrLifecycle, status)
	}

	tier := TierFromScore(score)
	now := nowUnix()

	_, err = tx.ExecContext(ctx,
		"UPDATE discoveries SET relevance_score = ?, confidence_tier = ?, status = ? WHERE id = ?",
		score, tier, StatusScored, id)
	if err != nil {
		return fmt.Errorf("score: update: %w", err)
	}

	// Emit scored event
	payload, _ := json.Marshal(map[string]interface{}{
		"id": id, "old_score": oldScore, "new_score": score, "tier": tier,
	})
	_, err = tx.ExecContext(ctx, `
		INSERT INTO discovery_events (discovery_id, event_type, from_status, to_status, payload, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		id, EventScored, status, StatusScored, string(payload), now)
	if err != nil {
		return fmt.Errorf("score: event: %w", err)
	}

	return tx.Commit()
}

// Promote promotes a discovery to a bead, enforcing the confidence gate.
// Returns ErrNotFound if missing, ErrGateBlocked if score too low, ErrLifecycle if dismissed.
func (s *Store) Promote(ctx context.Context, id, beadID string, force bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("promote: begin: %w", err)
	}
	defer tx.Rollback()

	// SELECT to verify existence and current state
	var status string
	var score float64
	err = tx.QueryRowContext(ctx, "SELECT status, relevance_score FROM discoveries WHERE id = ?", id).Scan(&status, &score)
	if err == sql.ErrNoRows {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return fmt.Errorf("promote: lookup: %w", err)
	}

	// Dismissed discoveries cannot be promoted, even with --force
	if status == StatusDismissed {
		return fmt.Errorf("%w: cannot promote dismissed discovery", ErrLifecycle)
	}

	// Already promoted — idempotent
	if status == StatusPromoted {
		return nil
	}

	// Gate check (skip if force)
	if !force && score < TierMediumMin {
		return fmt.Errorf("%w: confidence %.2f below promotion threshold %.2f", ErrGateBlocked, score, TierMediumMin)
	}

	now := nowUnix()
	_, err = tx.ExecContext(ctx,
		"UPDATE discoveries SET status = ?, bead_id = ?, promoted_at = ? WHERE id = ?",
		StatusPromoted, beadID, now, id)
	if err != nil {
		return fmt.Errorf("promote: update: %w", err)
	}

	// Emit promoted event
	payload, _ := json.Marshal(map[string]interface{}{
		"id": id, "bead_id": beadID, "score": score, "force": force,
	})
	_, err = tx.ExecContext(ctx, `
		INSERT INTO discovery_events (discovery_id, event_type, from_status, to_status, payload, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		id, EventPromoted, status, StatusPromoted, string(payload), now)
	if err != nil {
		return fmt.Errorf("promote: event: %w", err)
	}

	return tx.Commit()
}

// Dismiss marks a discovery as dismissed.
func (s *Store) Dismiss(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("dismiss: begin: %w", err)
	}
	defer tx.Rollback()

	// Verify existence
	var status string
	err = tx.QueryRowContext(ctx, "SELECT status FROM discoveries WHERE id = ?", id).Scan(&status)
	if err == sql.ErrNoRows {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return fmt.Errorf("dismiss: lookup: %w", err)
	}

	now := nowUnix()
	_, err = tx.ExecContext(ctx,
		"UPDATE discoveries SET status = ?, reviewed_at = ? WHERE id = ?",
		StatusDismissed, now, id)
	if err != nil {
		return fmt.Errorf("dismiss: update: %w", err)
	}

	payload, _ := json.Marshal(map[string]interface{}{"id": id})
	_, err = tx.ExecContext(ctx, `
		INSERT INTO discovery_events (discovery_id, event_type, from_status, to_status, payload, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		id, EventDismissed, status, StatusDismissed, string(payload), now)
	if err != nil {
		return fmt.Errorf("dismiss: event: %w", err)
	}

	return tx.Commit()
}

// RecordFeedback records a feedback signal and emits a feedback.recorded event.
func (s *Store) RecordFeedback(ctx context.Context, discoveryID, signalType, data, actor string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("feedback: begin: %w", err)
	}
	defer tx.Rollback()

	// Verify discovery exists
	var exists int
	err = tx.QueryRowContext(ctx, "SELECT 1 FROM discoveries WHERE id = ?", discoveryID).Scan(&exists)
	if err == sql.ErrNoRows {
		return fmt.Errorf("%w: %s", ErrNotFound, discoveryID)
	}
	if err != nil {
		return fmt.Errorf("feedback: lookup: %w", err)
	}

	now := nowUnix()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO feedback_signals (discovery_id, signal_type, signal_data, actor, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		discoveryID, signalType, data, actor, now)
	if err != nil {
		return fmt.Errorf("feedback: insert: %w", err)
	}

	payload, _ := json.Marshal(map[string]interface{}{
		"discovery_id": discoveryID, "signal_type": signalType,
	})
	_, err = tx.ExecContext(ctx, `
		INSERT INTO discovery_events (discovery_id, event_type, payload, created_at)
		VALUES (?, ?, ?, ?)`,
		discoveryID, EventFeedback, string(payload), now)
	if err != nil {
		return fmt.Errorf("feedback: event: %w", err)
	}

	return tx.Commit()
}

// GetProfile returns the current interest profile (singleton row).
// Returns a zero-value profile if none exists.
func (s *Store) GetProfile(ctx context.Context) (*InterestProfile, error) {
	var p InterestProfile
	err := s.db.QueryRowContext(ctx,
		"SELECT id, topic_vector, keyword_weights, source_weights, updated_at FROM interest_profile WHERE id = 1",
	).Scan(&p.ID, &p.TopicVector, &p.KeywordWeights, &p.SourceWeights, &p.UpdatedAt)
	if err == sql.ErrNoRows {
		return &InterestProfile{ID: 1, KeywordWeights: "{}", SourceWeights: "{}"}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get profile: %w", err)
	}
	return &p, nil
}

// UpdateProfile upserts the interest profile.
// Passing nil for topicVector leaves the existing embedding intact.
func (s *Store) UpdateProfile(ctx context.Context, topicVector []byte, keywordWeights, sourceWeights string) error {
	now := nowUnix()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO interest_profile (id, topic_vector, keyword_weights, source_weights, updated_at)
		VALUES (1, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			topic_vector = COALESCE(excluded.topic_vector, interest_profile.topic_vector),
			keyword_weights = excluded.keyword_weights,
			source_weights = excluded.source_weights,
			updated_at = excluded.updated_at`,
		topicVector, keywordWeights, sourceWeights, now)
	if err != nil {
		return fmt.Errorf("update profile: %w", err)
	}
	return nil
}

// Decay applies multiplicative decay to active discoveries older than minAgeSec.
// Tier is recomputed in Go via TierFromScore to avoid SQL parameter binding issues.
func (s *Store) Decay(ctx context.Context, rate float64, minAgeSec int64) (int, error) {
	cutoff := nowUnix() - minAgeSec

	// Load eligible discoveries
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, relevance_score FROM discoveries
		 WHERE discovered_at < ? AND status NOT IN ('dismissed', 'promoted')`,
		cutoff)
	if err != nil {
		return 0, fmt.Errorf("decay: query: %w", err)
	}
	defer rows.Close()

	type target struct {
		id    string
		score float64
	}
	var targets []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.id, &t.score); err != nil {
			return 0, fmt.Errorf("decay: scan: %w", err)
		}
		targets = append(targets, t)
	}
	rows.Close()

	if len(targets) == 0 {
		return 0, nil
	}

	// Apply decay and recompute tier in Go, write back in a single transaction
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("decay: begin: %w", err)
	}
	defer tx.Rollback()

	for _, t := range targets {
		newScore := t.score * (1.0 - rate)
		newTier := TierFromScore(newScore)
		_, err := tx.ExecContext(ctx,
			"UPDATE discoveries SET relevance_score = ?, confidence_tier = ? WHERE id = ?",
			newScore, newTier, t.id)
		if err != nil {
			return 0, fmt.Errorf("decay: update %s: %w", t.id, err)
		}
	}

	// Emit single decay event
	payload, _ := json.Marshal(map[string]interface{}{
		"count": len(targets), "rate": rate,
	})
	_, err = tx.ExecContext(ctx, `
		INSERT INTO discovery_events (discovery_id, event_type, payload, created_at)
		VALUES ('', ?, ?, ?)`,
		EventDecayed, string(payload), nowUnix())
	if err != nil {
		return 0, fmt.Errorf("decay: event: %w", err)
	}

	return len(targets), tx.Commit()
}

// SearchResult extends Discovery with a similarity score.
type SearchResult struct {
	Discovery
	Similarity float64 `json:"similarity"`
}

// SearchFilter controls what Search returns.
type SearchFilter struct {
	Source   string
	Tier     string
	Status   string
	MinScore float64
	Limit    int
}

// Search finds discoveries by embedding cosine similarity.
// Brute-force scan — sufficient for <10K rows.
func (s *Store) Search(ctx context.Context, queryEmbedding []byte, f SearchFilter) ([]SearchResult, error) {
	if f.Limit <= 0 {
		f.Limit = 10
	}

	query := "SELECT id, source, source_id, title, summary, url, relevance_score, confidence_tier, status, embedding, discovered_at FROM discoveries WHERE embedding IS NOT NULL"
	var args []interface{}
	if f.Source != "" {
		query += " AND source = ?"
		args = append(args, f.Source)
	}
	if f.Tier != "" {
		query += " AND confidence_tier = ?"
		args = append(args, f.Tier)
	}
	if f.Status != "" {
		query += " AND status = ?"
		args = append(args, f.Status)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var d Discovery
		var emb []byte
		if err := rows.Scan(&d.ID, &d.Source, &d.SourceID, &d.Title, &d.Summary, &d.URL,
			&d.RelevanceScore, &d.ConfidenceTier, &d.Status, &emb, &d.DiscoveredAt); err != nil {
			return nil, fmt.Errorf("search scan: %w", err)
		}
		sim := CosineSimilarity(queryEmbedding, emb)
		if f.MinScore > 0 && sim < f.MinScore {
			continue
		}
		results = append(results, SearchResult{Discovery: d, Similarity: sim})
	}

	// Sort by similarity DESC, id ASC (stable)
	sortSearchResults(results)

	if len(results) > f.Limit {
		results = results[:f.Limit]
	}
	return results, nil
}

// Rollback dismisses discoveries from a source since a timestamp.
// Uses UPDATE ... RETURNING id for atomic ID collection.
func (s *Store) Rollback(ctx context.Context, source string, sinceTimestamp int64) (int, error) {
	now := nowUnix()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("rollback: begin: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx,
		`UPDATE discoveries SET status = 'dismissed', reviewed_at = ?
		 WHERE source = ? AND discovered_at >= ? AND status NOT IN ('promoted', 'dismissed')
		 RETURNING id`, now, source, sinceTimestamp)
	if err != nil {
		return 0, fmt.Errorf("rollback: update: %w", err)
	}

	var affectedIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("rollback: scan: %w", err)
		}
		affectedIDs = append(affectedIDs, id)
	}
	rows.Close()

	// Emit dismissed event for each affected ID
	for _, id := range affectedIDs {
		payload, _ := json.Marshal(map[string]interface{}{
			"id": id, "reason": "rollback", "source": source,
		})
		_, err = tx.ExecContext(ctx, `
			INSERT INTO discovery_events (discovery_id, event_type, from_status, to_status, payload, created_at)
			VALUES (?, ?, '', 'dismissed', ?, ?)`,
			id, EventDismissed, string(payload), now)
		if err != nil {
			return 0, fmt.Errorf("rollback: event for %s: %w", id, err)
		}
	}

	return len(affectedIDs), tx.Commit()
}

// MaxEventID returns the maximum event ID for discovery events.
func (s *Store) MaxEventID(ctx context.Context) (int64, error) {
	var id sql.NullInt64
	err := s.db.QueryRowContext(ctx, "SELECT MAX(id) FROM discovery_events").Scan(&id)
	if err != nil {
		return 0, err
	}
	if !id.Valid {
		return 0, nil
	}
	return id.Int64, nil
}

func nowUnix() int64 {
	return time.Now().Unix()
}

func isUniqueConstraintError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// sortSearchResults sorts by similarity DESC, then id ASC for determinism.
func sortSearchResults(results []SearchResult) {
	for i := 1; i < len(results); i++ {
		for j := i; j > 0; j-- {
			if results[j].Similarity > results[j-1].Similarity ||
				(results[j].Similarity == results[j-1].Similarity && results[j].ID < results[j-1].ID) {
				results[j], results[j-1] = results[j-1], results[j]
			} else {
				break
			}
		}
	}
}
