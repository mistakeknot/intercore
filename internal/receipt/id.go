package receipt

import (
	"crypto/rand"
	"time"

	"github.com/oklog/ulid/v2"
)

// NewID returns a receipt_id of the form rcpt_<26-char ULID> per
// canon §Receipt schema. ULIDs encode the timestamp in their first 48 bits,
// so receipts sort chronologically by ID.
func NewID(now time.Time) string {
	return IDPrefix + ulid.MustNew(ulid.Timestamp(now), rand.Reader).String()
}

// FormatTimestamp returns an RFC 3339 UTC timestamp with microsecond
// precision, matching canon §Receipt schema "Example: 2026-05-23T19:42:01.234567Z".
//
// Go's time.RFC3339Nano produces nanosecond precision and trims trailing
// zeros; the canon spec pins microseconds (6 decimal places, never trimmed)
// so verifiers can byte-compare timestamps without parsing.
func FormatTimestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000000Z")
}
