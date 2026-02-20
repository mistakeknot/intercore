package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

const (
	maxPayloadSize  = 1 << 20       // 1MB
	maxNestingDepth = 20            // max JSON nesting
	maxKeyLength    = 1000          // max JSON object key length
	maxStringLength = 100 * 1024    // 100KB per string value
	maxArrayLength  = 10000         // max array elements
)

var ErrNotFound = errors.New("not found")

// Store provides state operations against the intercore DB.
type Store struct {
	db *sql.DB
}

// New creates a state store.
func New(db *sql.DB) *Store {
	return &Store{db: db}
}

// Set writes a state entry. TTL of 0 means no expiration.
func (s *Store) Set(ctx context.Context, key, scopeID string, payload json.RawMessage, ttl time.Duration) error {
	if err := ValidatePayload(payload); err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("state set: begin: %w", err)
	}
	defer tx.Rollback()

	var expiresAt *int64
	if ttl > 0 {
		seconds := int64(ttl.Seconds())
		if seconds < 1 {
			return fmt.Errorf("state set: TTL must be at least 1 second (got %v)", ttl)
		}
		ea := time.Now().Unix() + seconds
		expiresAt = &ea
	}

	_, err = tx.ExecContext(ctx,
		`INSERT OR REPLACE INTO state (key, scope_id, payload, updated_at, expires_at)
		 VALUES (?, ?, ?, unixepoch(), ?)`,
		key, scopeID, string(payload), expiresAt)
	if err != nil {
		return fmt.Errorf("state set: insert: %w", err)
	}

	return tx.Commit()
}

// Get reads a state entry. Returns ErrNotFound if not present or expired.
func (s *Store) Get(ctx context.Context, key, scopeID string) (json.RawMessage, error) {
	var payload string
	err := s.db.QueryRowContext(ctx,
		`SELECT payload FROM state
		 WHERE key = ? AND scope_id = ?
		   AND (expires_at IS NULL OR expires_at > unixepoch())`,
		key, scopeID).Scan(&payload)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("state get: %w", err)
	}
	return json.RawMessage(payload), nil
}

// Delete removes a state entry. Returns true if a row was deleted.
func (s *Store) Delete(ctx context.Context, key, scopeID string) (bool, error) {
	result, err := s.db.ExecContext(ctx,
		"DELETE FROM state WHERE key = ? AND scope_id = ?",
		key, scopeID)
	if err != nil {
		return false, fmt.Errorf("state delete: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("state delete: %w", err)
	}
	return n > 0, nil
}

// List returns all scope_ids for a given key (excluding expired).
func (s *Store) List(ctx context.Context, key string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT scope_id FROM state
		 WHERE key = ? AND (expires_at IS NULL OR expires_at > unixepoch())
		 ORDER BY scope_id`,
		key)
	if err != nil {
		return nil, fmt.Errorf("state list: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("state list scan: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// Prune deletes expired state rows.
func (s *Store) Prune(ctx context.Context) (int64, error) {
	result, err := s.db.ExecContext(ctx,
		"DELETE FROM state WHERE expires_at IS NOT NULL AND expires_at <= unixepoch()")
	if err != nil {
		return 0, fmt.Errorf("state prune: %w", err)
	}
	return result.RowsAffected()
}

// secretPatterns matches common secret formats that should never be stored
// in the kernel database. The kernel stores provenance (variable names,
// references) — never actual secret values.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^(sk|pk|rk|ak)[-_][a-zA-Z0-9]{20,}`),            // API key prefixes (OpenAI sk-, Stripe pk_/sk_, etc.)
	regexp.MustCompile(`(?i)^(ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9_]{30,}`),      // GitHub tokens
	regexp.MustCompile(`(?i)^xox[bpras]-[A-Za-z0-9-]{20,}`),                  // Slack tokens
	regexp.MustCompile(`(?i)^eyJ[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{20,}\.`),  // JWT tokens
	regexp.MustCompile(`(?i)^AKIA[0-9A-Z]{16}`),                              // AWS access key IDs
	regexp.MustCompile(`(?i)^-----BEGIN (RSA |EC |DSA |OPENSSH )?PRIVATE KEY`), // PEM private keys
	regexp.MustCompile(`(?i)(password|passwd|secret|token|api_key|apikey|api-key|access_key|private_key)\s*[:=]\s*\S{8,}`), // key=value assignments
}

// containsSecret checks whether a string value looks like a secret.
func containsSecret(s string) bool {
	for _, p := range secretPatterns {
		if p.MatchString(s) {
			return true
		}
	}
	return false
}

// ValidatePayload checks JSON validity, size, depth, structure limits,
// and rejects payloads containing likely secret values.
func ValidatePayload(data []byte) error {
	if len(data) > maxPayloadSize {
		return fmt.Errorf("payload too large: %d bytes (max %d)", len(data), maxPayloadSize)
	}
	if !json.Valid(data) {
		return fmt.Errorf("invalid JSON payload")
	}
	if err := validateSecrets(data); err != nil {
		return err
	}
	return validateDepth(data)
}

func validateDepth(data []byte) error {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	depth := 0
	// Stack tracks array element counts at each nesting level.
	// A value of -1 means the current level is an object, not an array.
	var arrayStack []int

	for {
		tok, err := dec.Token()
		if err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("JSON parse error: %w", err)
		}

		switch v := tok.(type) {
		case json.Delim:
			switch v {
			case '{':
				depth++
				if depth > maxNestingDepth {
					return fmt.Errorf("JSON nesting too deep: >%d levels", maxNestingDepth)
				}
				arrayStack = append(arrayStack, -1) // object marker
			case '[':
				depth++
				if depth > maxNestingDepth {
					return fmt.Errorf("JSON nesting too deep: >%d levels", maxNestingDepth)
				}
				arrayStack = append(arrayStack, 0) // array with 0 elements
			case '}', ']':
				depth--
				if len(arrayStack) > 0 {
					arrayStack = arrayStack[:len(arrayStack)-1]
				}
			}
		case string:
			if len(v) > maxStringLength {
				return fmt.Errorf("JSON string value too long: %d bytes (max %d)", len(v), maxStringLength)
			}
			if n := len(arrayStack); n > 0 && arrayStack[n-1] >= 0 {
				arrayStack[n-1]++
				if arrayStack[n-1] > maxArrayLength {
					return fmt.Errorf("JSON array too long: >%d elements", maxArrayLength)
				}
			}
		default:
			if n := len(arrayStack); n > 0 && arrayStack[n-1] >= 0 {
				arrayStack[n-1]++
				if arrayStack[n-1] > maxArrayLength {
					return fmt.Errorf("JSON array too long: >%d elements", maxArrayLength)
				}
			}
		}
	}
	return nil
}

// validateSecrets walks all string values in the JSON and rejects any
// that match common secret patterns. This enforces the no-secrets-in-DB
// invariant from the intercore security baseline.
func validateSecrets(data []byte) error {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	isKey := false
	for {
		tok, err := dec.Token()
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil // let validateDepth handle parse errors
		}
		switch v := tok.(type) {
		case json.Delim:
			isKey = v == '{'
		case string:
			if isKey {
				// This is an object key — skip it, check the next value
				isKey = false
				continue
			}
			if containsSecret(v) {
				// Truncate the value for the error message
				preview := v
				if len(preview) > 20 {
					preview = preview[:20] + "..."
				}
				return fmt.Errorf("payload contains a likely secret value (%q); store variable names for provenance, not secret values", preview)
			}
			isKey = false
		default:
			isKey = false
		}
	}
	return nil
}
