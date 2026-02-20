package discovery

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/mistakeknot/interverse/infra/intercore/internal/db"
)

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".clavain", "intercore.db")
	os.MkdirAll(filepath.Dir(dbPath), 0700)

	d, err := db.Open(dbPath, 0)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := d.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d.SqlDB()
}

func mustSubmit(t *testing.T, s *Store, source, sourceID, title string, score float64) string {
	t.Helper()
	id, err := s.Submit(context.Background(), source, sourceID, title, "", "", "{}", nil, score)
	if err != nil {
		t.Fatalf("submit %s/%s: %v", source, sourceID, err)
	}
	return id
}

func TestSubmitAndGet(t *testing.T) {
	sqlDB := setupTestDB(t)
	s := NewStore(sqlDB)
	ctx := context.Background()

	id, err := s.Submit(ctx, "arxiv", "2401.12345", "Attention Is All You Need v2", "A followup paper", "https://arxiv.org/abs/2401.12345", "{}", nil, 0.7)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if id == "" {
		t.Fatal("submit returned empty ID")
	}

	d, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if d.Source != "arxiv" {
		t.Errorf("source: got %q, want %q", d.Source, "arxiv")
	}
	if d.ConfidenceTier != TierMedium {
		t.Errorf("tier: got %q, want %q (score 0.7)", d.ConfidenceTier, TierMedium)
	}
	if d.Status != StatusNew {
		t.Errorf("status: got %q, want %q", d.Status, StatusNew)
	}
}

func TestGetNotFound(t *testing.T) {
	sqlDB := setupTestDB(t)
	s := NewStore(sqlDB)
	ctx := context.Background()

	_, err := s.Get(ctx, "nonexistent")
	if err == nil {
		t.Fatal("expected error for missing ID")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got: %v", err)
	}
}

func TestSubmitDuplicateSourceID(t *testing.T) {
	sqlDB := setupTestDB(t)
	s := NewStore(sqlDB)
	ctx := context.Background()

	_, err := s.Submit(ctx, "arxiv", "dup-1", "First", "", "", "{}", nil, 0.5)
	if err != nil {
		t.Fatalf("first submit: %v", err)
	}

	_, err = s.Submit(ctx, "arxiv", "dup-1", "Second", "", "", "{}", nil, 0.5)
	if err == nil {
		t.Fatal("expected duplicate constraint error, got nil")
	}
	if !errors.Is(err, ErrDuplicate) {
		t.Errorf("expected ErrDuplicate, got: %v", err)
	}
}

func TestList(t *testing.T) {
	sqlDB := setupTestDB(t)
	s := NewStore(sqlDB)
	ctx := context.Background()

	mustSubmit(t, s, "arxiv", "a1", "Paper A", 0.9)
	mustSubmit(t, s, "hackernews", "h1", "HN Post", 0.4)
	mustSubmit(t, s, "arxiv", "a2", "Paper B", 0.6)

	// List all
	results, err := s.List(ctx, ListFilter{Limit: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("list count: got %d, want 3", len(results))
	}
	// Should be sorted by relevance_score DESC
	if results[0].RelevanceScore < results[1].RelevanceScore {
		t.Error("list not sorted by relevance_score DESC")
	}

	// Filter by source
	results, err = s.List(ctx, ListFilter{Source: "arxiv", Limit: 10})
	if err != nil {
		t.Fatalf("list source: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("list source count: got %d, want 2", len(results))
	}

	// Filter by tier
	results, err = s.List(ctx, ListFilter{Tier: TierHigh, Limit: 10})
	if err != nil {
		t.Fatalf("list tier: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("list tier count: got %d, want 1", len(results))
	}
}

func TestScore(t *testing.T) {
	sqlDB := setupTestDB(t)
	s := NewStore(sqlDB)
	ctx := context.Background()

	id := mustSubmit(t, s, "arxiv", "s1", "Paper", 0.3)

	err := s.Score(ctx, id, 0.85)
	if err != nil {
		t.Fatalf("score: %v", err)
	}

	d, _ := s.Get(ctx, id)
	if d.RelevanceScore != 0.85 {
		t.Errorf("score: got %f, want 0.85", d.RelevanceScore)
	}
	if d.ConfidenceTier != TierHigh {
		t.Errorf("tier: got %q, want %q", d.ConfidenceTier, TierHigh)
	}
}

func TestScoreNotFound(t *testing.T) {
	sqlDB := setupTestDB(t)
	s := NewStore(sqlDB)
	ctx := context.Background()

	err := s.Score(ctx, "nonexistent", 0.9)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got: %v", err)
	}
}

func TestScoreDismissedDiscovery(t *testing.T) {
	sqlDB := setupTestDB(t)
	s := NewStore(sqlDB)
	ctx := context.Background()

	id := mustSubmit(t, s, "arxiv", "sd1", "Paper", 0.5)
	_ = s.Dismiss(ctx, id)

	err := s.Score(ctx, id, 0.95)
	if err == nil {
		t.Fatal("expected lifecycle error for scoring dismissed discovery")
	}
	if !errors.Is(err, ErrLifecycle) {
		t.Errorf("expected ErrLifecycle, got: %v", err)
	}
}

func TestPromote(t *testing.T) {
	sqlDB := setupTestDB(t)
	s := NewStore(sqlDB)
	ctx := context.Background()

	id := mustSubmit(t, s, "arxiv", "p1", "Paper", 0.7)
	beadID := "iv-test1"

	err := s.Promote(ctx, id, beadID, false)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}

	d, _ := s.Get(ctx, id)
	if d.Status != StatusPromoted {
		t.Errorf("status: got %q, want %q", d.Status, StatusPromoted)
	}
	if d.BeadID == nil || *d.BeadID != beadID {
		t.Errorf("bead_id: got %v, want %q", d.BeadID, beadID)
	}
}

func TestPromoteGateBlock(t *testing.T) {
	sqlDB := setupTestDB(t)
	s := NewStore(sqlDB)
	ctx := context.Background()

	id := mustSubmit(t, s, "arxiv", "g1", "Low Score Paper", 0.2)

	err := s.Promote(ctx, id, "iv-test2", false)
	if err == nil {
		t.Fatal("expected gate block error, got nil")
	}
	if !errors.Is(err, ErrGateBlocked) {
		t.Errorf("expected ErrGateBlocked, got: %v", err)
	}
}

func TestPromoteForceOverride(t *testing.T) {
	sqlDB := setupTestDB(t)
	s := NewStore(sqlDB)
	ctx := context.Background()

	id := mustSubmit(t, s, "arxiv", "f1", "Low Score Paper", 0.2)

	err := s.Promote(ctx, id, "iv-test3", true)
	if err != nil {
		t.Fatalf("force promote: %v", err)
	}

	d, _ := s.Get(ctx, id)
	if d.Status != StatusPromoted {
		t.Errorf("status: got %q, want promoted", d.Status)
	}
}

func TestPromoteNotFound(t *testing.T) {
	sqlDB := setupTestDB(t)
	s := NewStore(sqlDB)
	ctx := context.Background()

	err := s.Promote(ctx, "nonexistent-id", "iv-test", false)
	if err == nil {
		t.Fatal("expected not found error")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got: %v", err)
	}
}

func TestPromoteDismissedBlocked(t *testing.T) {
	sqlDB := setupTestDB(t)
	s := NewStore(sqlDB)
	ctx := context.Background()

	id := mustSubmit(t, s, "arxiv", "pd1", "Paper", 0.7)
	_ = s.Dismiss(ctx, id)

	// Even with --force, dismissed discoveries cannot be promoted
	err := s.Promote(ctx, id, "iv-test", true)
	if err == nil {
		t.Fatal("expected lifecycle error for promoting dismissed discovery")
	}
	if !errors.Is(err, ErrLifecycle) {
		t.Errorf("expected ErrLifecycle, got: %v", err)
	}
}

func TestDismiss(t *testing.T) {
	sqlDB := setupTestDB(t)
	s := NewStore(sqlDB)
	ctx := context.Background()

	id := mustSubmit(t, s, "arxiv", "d1", "Paper", 0.5)
	err := s.Dismiss(ctx, id)
	if err != nil {
		t.Fatalf("dismiss: %v", err)
	}

	d, _ := s.Get(ctx, id)
	if d.Status != StatusDismissed {
		t.Errorf("status: got %q, want dismissed", d.Status)
	}
}

func TestRecordFeedback(t *testing.T) {
	sqlDB := setupTestDB(t)
	s := NewStore(sqlDB)
	ctx := context.Background()

	id := mustSubmit(t, s, "test", "fb1", "Paper", 0.5)

	err := s.RecordFeedback(ctx, id, SignalBoost, "{}", "human")
	if err != nil {
		t.Fatalf("record feedback: %v", err)
	}
}

func TestRecordFeedbackNotFound(t *testing.T) {
	sqlDB := setupTestDB(t)
	s := NewStore(sqlDB)
	ctx := context.Background()

	err := s.RecordFeedback(ctx, "nonexistent", SignalBoost, "{}", "human")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got: %v", err)
	}
}

func TestInterestProfile(t *testing.T) {
	sqlDB := setupTestDB(t)
	s := NewStore(sqlDB)
	ctx := context.Background()

	err := s.UpdateProfile(ctx, nil, `{"ai":0.8,"security":0.5}`, `{"arxiv":0.9}`)
	if err != nil {
		t.Fatalf("update profile: %v", err)
	}

	p, err := s.GetProfile(ctx)
	if err != nil {
		t.Fatalf("get profile: %v", err)
	}
	if p.KeywordWeights != `{"ai":0.8,"security":0.5}` {
		t.Errorf("keyword weights: got %q", p.KeywordWeights)
	}
}

func TestProfilePreservesTopicVector(t *testing.T) {
	sqlDB := setupTestDB(t)
	s := NewStore(sqlDB)
	ctx := context.Background()

	// Set profile with topic vector
	vec := []byte{1, 2, 3, 4}
	err := s.UpdateProfile(ctx, vec, `{"ai":0.8}`, `{"arxiv":0.9}`)
	if err != nil {
		t.Fatalf("first update: %v", err)
	}

	// Update without topic vector (nil) — should preserve existing
	err = s.UpdateProfile(ctx, nil, `{"ai":0.9}`, `{"arxiv":0.8}`)
	if err != nil {
		t.Fatalf("second update: %v", err)
	}

	p, _ := s.GetProfile(ctx)
	if len(p.TopicVector) != 4 {
		t.Errorf("topic vector lost: got len=%d, want 4", len(p.TopicVector))
	}
	if p.KeywordWeights != `{"ai":0.9}` {
		t.Errorf("keywords not updated: got %q", p.KeywordWeights)
	}
}

func TestCosineSimilarity(t *testing.T) {
	a := Float32ToBytes([]float32{1.0, 0.0, 0.0, 0.0})
	b := Float32ToBytes([]float32{1.0, 0.0, 0.0, 0.0})
	c := Float32ToBytes([]float32{0.0, 1.0, 0.0, 0.0})

	if sim := CosineSimilarity(a, b); sim < 0.99 {
		t.Errorf("identical vectors: got %f, want ~1.0", sim)
	}
	if sim := CosineSimilarity(a, c); sim > 0.01 {
		t.Errorf("orthogonal vectors: got %f, want ~0.0", sim)
	}
	if sim := CosineSimilarity(nil, a); sim != 0.0 {
		t.Errorf("nil input: got %f, want 0.0", sim)
	}
}

func TestSubmitDedup(t *testing.T) {
	sqlDB := setupTestDB(t)
	s := NewStore(sqlDB)
	ctx := context.Background()

	emb := Float32ToBytes([]float32{1.0, 0.0, 0.0, 0.0})
	id1, _ := s.Submit(ctx, "test", "dd1", "Paper A", "", "", "{}", emb, 0.5)

	// Submit near-duplicate with high threshold
	emb2 := Float32ToBytes([]float32{0.99, 0.01, 0.0, 0.0})
	id2, err := s.SubmitWithDedup(ctx, "test", "dd2", "Paper A Similar", "", "", "{}", emb2, 0.5, 0.9)
	if err != nil {
		t.Fatalf("dedup submit: %v", err)
	}
	// Should return the original ID (dedup hit)
	if id2 != id1 {
		t.Errorf("dedup: got %q, want %q (original)", id2, id1)
	}
}

func TestSubmitDedupMiss(t *testing.T) {
	sqlDB := setupTestDB(t)
	s := NewStore(sqlDB)
	ctx := context.Background()

	emb := Float32ToBytes([]float32{1.0, 0.0, 0.0, 0.0})
	id1, _ := s.Submit(ctx, "test", "dm1", "Paper A", "", "", "{}", emb, 0.5)

	// Submit very different vector — should NOT dedup
	emb2 := Float32ToBytes([]float32{0.0, 1.0, 0.0, 0.0})
	id2, err := s.SubmitWithDedup(ctx, "test", "dm2", "Paper B", "", "", "{}", emb2, 0.5, 0.9)
	if err != nil {
		t.Fatalf("dedup miss submit: %v", err)
	}
	if id2 == id1 {
		t.Error("expected different ID for non-duplicate")
	}
}

func TestDecay(t *testing.T) {
	sqlDB := setupTestDB(t)
	s := NewStore(sqlDB)
	ctx := context.Background()

	id := mustSubmit(t, s, "test", "dc1", "Old Paper", 0.8)

	// Force discovered_at to be old
	sqlDB.ExecContext(ctx, "UPDATE discoveries SET discovered_at = ? WHERE id = ?", nowUnix()-86400*30, id)

	count, err := s.Decay(ctx, 0.1, 86400) // 10% decay, min age 1 day
	if err != nil {
		t.Fatalf("decay: %v", err)
	}
	if count != 1 {
		t.Errorf("decay count: got %d, want 1", count)
	}

	d, _ := s.Get(ctx, id)
	if d.RelevanceScore >= 0.8 {
		t.Errorf("score should have decayed from 0.8, got %f", d.RelevanceScore)
	}
	// Verify tier is consistent with decayed score
	expectedTier := TierFromScore(d.RelevanceScore)
	if d.ConfidenceTier != expectedTier {
		t.Errorf("tier mismatch after decay: got %q, want %q (score=%f)", d.ConfidenceTier, expectedTier, d.RelevanceScore)
	}
}

func TestDecaySkipsDismissed(t *testing.T) {
	sqlDB := setupTestDB(t)
	s := NewStore(sqlDB)
	ctx := context.Background()

	id := mustSubmit(t, s, "test", "dcd1", "Dismissed Paper", 0.8)
	_ = s.Dismiss(ctx, id)

	// Force old timestamp
	sqlDB.ExecContext(ctx, "UPDATE discoveries SET discovered_at = ? WHERE id = ?", nowUnix()-86400*30, id)

	count, _ := s.Decay(ctx, 0.5, 86400)
	if count != 0 {
		t.Errorf("decay should skip dismissed, got count=%d", count)
	}
}

func TestSearch(t *testing.T) {
	sqlDB := setupTestDB(t)
	s := NewStore(sqlDB)
	ctx := context.Background()

	emb1 := Float32ToBytes([]float32{1.0, 0.0, 0.0, 0.0})
	emb2 := Float32ToBytes([]float32{0.0, 1.0, 0.0, 0.0})
	emb3 := Float32ToBytes([]float32{0.9, 0.1, 0.0, 0.0})

	s.Submit(ctx, "test", "s1", "Paper 1", "", "", "{}", emb1, 0.8)
	s.Submit(ctx, "test", "s2", "Paper 2", "", "", "{}", emb2, 0.7)
	s.Submit(ctx, "test", "s3", "Paper 3", "", "", "{}", emb3, 0.6)

	query := Float32ToBytes([]float32{1.0, 0.0, 0.0, 0.0})
	results, err := s.Search(ctx, query, SearchFilter{Limit: 2})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("search count: got %d, want 2", len(results))
	}
	// First result should be the most similar (Paper 1)
	if results[0].Title != "Paper 1" {
		t.Errorf("first result: got %q, want Paper 1", results[0].Title)
	}
}

func TestRollback(t *testing.T) {
	sqlDB := setupTestDB(t)
	s := NewStore(sqlDB)
	ctx := context.Background()

	id1 := mustSubmit(t, s, "rollback-src", "r1", "R1", 0.5)
	id2 := mustSubmit(t, s, "rollback-src", "r2", "R2", 0.6)
	mustSubmit(t, s, "other-src", "o1", "Other", 0.7) // Different source — should not be affected

	count, err := s.Rollback(ctx, "rollback-src", 0)
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if count != 2 {
		t.Errorf("rollback count: got %d, want 2", count)
	}

	d1, _ := s.Get(ctx, id1)
	if d1.Status != StatusDismissed {
		t.Errorf("r1 status: got %q, want dismissed", d1.Status)
	}
	d2, _ := s.Get(ctx, id2)
	if d2.Status != StatusDismissed {
		t.Errorf("r2 status: got %q, want dismissed", d2.Status)
	}
}

func TestTierFromScore(t *testing.T) {
	tests := []struct {
		score float64
		want  string
	}{
		{1.0, TierHigh},
		{0.8, TierHigh},
		{0.79, TierMedium},
		{0.5, TierMedium},
		{0.49, TierLow},
		{0.3, TierLow},
		{0.29, TierDiscard},
		{0.0, TierDiscard},
	}
	for _, tt := range tests {
		got := TierFromScore(tt.score)
		if got != tt.want {
			t.Errorf("TierFromScore(%f) = %q, want %q", tt.score, got, tt.want)
		}
	}
}
