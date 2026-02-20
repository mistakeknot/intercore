package discovery

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"math"
	"math/big"
)

const (
	idLen   = 12
	idChars = "abcdefghijklmnopqrstuvwxyz0123456789"
)

// Status constants for discovery lifecycle.
const (
	StatusNew       = "new"
	StatusScored    = "scored"
	StatusPromoted  = "promoted"
	StatusProposed  = "proposed"
	StatusDismissed = "dismissed"
)

// Tier constants for confidence classification.
const (
	TierHigh    = "high"
	TierMedium  = "medium"
	TierLow     = "low"
	TierDiscard = "discard"
)

// Tier boundaries (score thresholds).
const (
	TierHighMin   = 0.8
	TierMediumMin = 0.5
	TierLowMin    = 0.3
)

// Signal types for feedback.
const (
	SignalPromote        = "promote"
	SignalDismiss        = "dismiss"
	SignalAdjustPriority = "adjust_priority"
	SignalBoost          = "boost"
	SignalPenalize       = "penalize"
)

// Event types for discovery events.
const (
	EventSubmitted = "discovery.submitted"
	EventScored    = "discovery.scored"
	EventPromoted  = "discovery.promoted"
	EventProposed  = "discovery.proposed"
	EventDismissed = "discovery.dismissed"
	EventDecayed   = "discovery.decayed"
	EventDeduped   = "discovery.deduped"
	EventFeedback  = "feedback.recorded"
)

// Discovery represents a research finding tracked in the kernel.
type Discovery struct {
	ID             string  `json:"id"`
	Source         string  `json:"source"`
	SourceID       string  `json:"source_id"`
	Title          string  `json:"title"`
	Summary        string  `json:"summary,omitempty"`
	URL            string  `json:"url,omitempty"`
	RawMetadata    string  `json:"raw_metadata,omitempty"`
	Embedding      []byte  `json:"-"` // BLOB, not serialized to JSON
	RelevanceScore float64 `json:"relevance_score"`
	ConfidenceTier string  `json:"confidence_tier"`
	Status         string  `json:"status"`
	RunID          *string `json:"run_id,omitempty"`
	BeadID         *string `json:"bead_id,omitempty"`
	DiscoveredAt   int64   `json:"discovered_at"`
	PromotedAt     *int64  `json:"promoted_at,omitempty"`
	ReviewedAt     *int64  `json:"reviewed_at,omitempty"`
}

// FeedbackSignal represents a feedback event on a discovery.
type FeedbackSignal struct {
	ID          int64  `json:"id"`
	DiscoveryID string `json:"discovery_id"`
	SignalType  string `json:"signal_type"`
	SignalData  string `json:"signal_data,omitempty"`
	Actor       string `json:"actor"`
	CreatedAt   int64  `json:"created_at"`
}

// InterestProfile represents the learned interest model (singleton row).
type InterestProfile struct {
	ID             int    `json:"id"`
	TopicVector    []byte `json:"-"` // BLOB
	KeywordWeights string `json:"keyword_weights"`
	SourceWeights  string `json:"source_weights"`
	UpdatedAt      int64  `json:"updated_at"`
}

// DiscoveryEvent represents a discovery lifecycle event.
// No Timestamp time.Time field — matches event.Event and event.InterspectEvent
// which use only integer timestamps. Callers convert at scan time if needed.
type DiscoveryEvent struct {
	ID          int64  `json:"id"`
	DiscoveryID string `json:"discovery_id"`
	EventType   string `json:"event_type"`
	FromStatus  string `json:"from_status"`
	ToStatus    string `json:"to_status"`
	Payload     string `json:"payload,omitempty"`
	CreatedAt   int64  `json:"created_at"`
}

// TierFromScore computes the confidence tier for a given score.
func TierFromScore(score float64) string {
	switch {
	case score >= TierHighMin:
		return TierHigh
	case score >= TierMediumMin:
		return TierMedium
	case score >= TierLowMin:
		return TierLow
	default:
		return TierDiscard
	}
}

// CosineSimilarity computes cosine similarity between two float32 BLOB embeddings.
// Returns 0.0 if either is nil or lengths don't match.
// Assumes little-endian byte order (standard on x86/ARM Linux).
func CosineSimilarity(a, b []byte) float64 {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) || len(a)%4 != 0 {
		return 0.0
	}
	dim := len(a) / 4
	var dotProduct, normA, normB float64
	for i := 0; i < dim; i++ {
		va := math.Float32frombits(binary.LittleEndian.Uint32(a[i*4 : (i+1)*4]))
		vb := math.Float32frombits(binary.LittleEndian.Uint32(b[i*4 : (i+1)*4]))
		dotProduct += float64(va) * float64(vb)
		normA += float64(va) * float64(va)
		normB += float64(vb) * float64(vb)
	}
	if normA == 0 || normB == 0 {
		return 0.0
	}
	return dotProduct / (math.Sqrt(normA) * math.Sqrt(normB))
}

// Float32ToBytes converts a float32 slice to little-endian bytes (for tests).
func Float32ToBytes(v []float32) []byte {
	buf := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf
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
