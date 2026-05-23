package receipt

import (
	"bytes"
	"strconv"
	"unicode/utf8"
)

// Canonicalize returns the byte stream over which the HMAC is computed,
// honoring the strict field order and encoding rules from
// docs/canon/signed-receipts-v1.md §Canonicalization.
//
// The output is deterministic: any two callers that agree on the receipt's
// signed fields produce byte-identical canonical streams. The unsigned
// envelope (Signature, SignatureAlg, KeyID, SignedAt) is excluded by
// construction — this function only reads the 8 signed fields.
//
// The canonical form is a JSON object with:
//   - keys in canon-declared order (NOT alphabetical, NOT struct-declared);
//   - no whitespace between tokens;
//   - integer numbers in plain decimal, no leading zeros, no exponent;
//   - strings using a minimal JSON escape set (\", \\, \n, \r, \t, \b, \f,
//     \u00XX for other control characters), no HTML-safe escapes;
//   - inner tool_calls[] objects with keys in canon order
//     (name, args_hash, result_hash, duration_ms);
//   - a nullable parent_run_id encoded as the literal null when nil.
func Canonicalize(r *Receipt) []byte {
	var b bytes.Buffer
	b.Grow(256)
	b.WriteByte('{')
	writeKey(&b, "receipt_id")
	writeString(&b, r.ReceiptID)
	b.WriteByte(',')
	writeKey(&b, "timestamp")
	writeString(&b, r.Timestamp)
	b.WriteByte(',')
	writeKey(&b, "agent_id")
	writeString(&b, r.AgentID)
	b.WriteByte(',')
	writeKey(&b, "model")
	writeString(&b, r.Model)
	b.WriteByte(',')
	writeKey(&b, "tool_calls")
	writeToolCalls(&b, r.ToolCalls)
	b.WriteByte(',')
	writeKey(&b, "parent_run_id")
	writeNullableString(&b, r.ParentRunID)
	b.WriteByte(',')
	writeKey(&b, "content_hash")
	writeString(&b, r.ContentHash)
	b.WriteByte(',')
	writeKey(&b, "schema_version")
	writeInt(&b, int64(r.SchemaVersion))
	b.WriteByte('}')
	return b.Bytes()
}

func writeKey(b *bytes.Buffer, k string) {
	writeString(b, k)
	b.WriteByte(':')
}

func writeToolCalls(b *bytes.Buffer, calls []ToolCall) {
	b.WriteByte('[')
	for i := range calls {
		c := &calls[i]
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('{')
		writeKey(b, "name")
		writeString(b, c.Name)
		b.WriteByte(',')
		writeKey(b, "args_hash")
		writeString(b, c.ArgsHash)
		b.WriteByte(',')
		writeKey(b, "result_hash")
		writeString(b, c.ResultHash)
		b.WriteByte(',')
		writeKey(b, "duration_ms")
		writeInt(b, c.DurationMs)
		b.WriteByte('}')
	}
	b.WriteByte(']')
}

func writeNullableString(b *bytes.Buffer, s *string) {
	if s == nil {
		b.WriteString("null")
		return
	}
	writeString(b, *s)
}

func writeInt(b *bytes.Buffer, n int64) {
	b.WriteString(strconv.FormatInt(n, 10))
}

const hexDigits = "0123456789abcdef"

// writeString writes s as a canonical JSON string per canon §Canonicalization.
// Required escapes: \", \\, \n, \r, \t, \b, \f and \u00XX for other control
// chars. Non-ASCII runes pass through as their UTF-8 bytes — JSON permits
// raw UTF-8 inside string literals and the canon doc imposes no further
// transformation. HTML-safe escapes (used by encoding/json by default for
// '<', '>', '&') are NOT emitted.
func writeString(b *bytes.Buffer, s string) {
	b.WriteByte('"')
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if r < 0x20 {
				b.WriteString(`\u00`)
				b.WriteByte(hexDigits[(r>>4)&0xf])
				b.WriteByte(hexDigits[r&0xf])
			} else {
				b.WriteString(s[i : i+size])
			}
		}
		i += size
	}
	b.WriteByte('"')
}
