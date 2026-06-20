package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/mistakeknot/intercore/internal/receipt"
)

func TestParseSinceDuration(t *testing.T) {
	tests := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"1d", 24 * time.Hour, false},
		{"7d", 7 * 24 * time.Hour, false},
		{"2w", 14 * 24 * time.Hour, false},
		{"0.5d", 12 * time.Hour, false},
		{"24h", 24 * time.Hour, false},
		{"90m", 90 * time.Minute, false},
		{"", 0, true},
		{"xd", 0, true},
		{"garbage", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := parseSinceDuration(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseSinceDuration(%q) = %v, want error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSinceDuration(%q) error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Fatalf("parseSinceDuration(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestDefaultEpoch(t *testing.T) {
	tests := []struct {
		month time.Month
		want  string
	}{
		{time.January, "2026-q1"}, {time.March, "2026-q1"},
		{time.April, "2026-q2"}, {time.June, "2026-q2"},
		{time.July, "2026-q3"}, {time.September, "2026-q3"},
		{time.October, "2026-q4"}, {time.December, "2026-q4"},
	}
	for _, tt := range tests {
		got := defaultEpoch(time.Date(2026, tt.month, 15, 0, 0, 0, 0, time.UTC))
		if got != tt.want {
			t.Errorf("defaultEpoch(%s) = %q, want %q", tt.month, got, tt.want)
		}
	}
}

// TestVerifyVerdict covers the verdict → canon-exit-code mapping that is the
// core of `ic receipt verify` (acceptance criteria #1 and #3 of
// sylveste-ewy3.5.3). It exercises the pure mapping without a DB or the CLI
// global flags, so each verdict path is asserted in isolation.
func TestVerifyVerdict(t *testing.T) {
	ks := receipt.NewMemKeyStore()
	const agent = "sylveste://agent/test"
	ks.Register(agent, "2026-q2", bytes.Repeat([]byte{0x11}, 32))

	base := receipt.Receipt{
		ReceiptID:     "rcpt_01ARZ3NDEKTSV4RRFFQ69G5FAV",
		Timestamp:     receipt.FormatTimestamp(time.Unix(1_700_000_000, 0)),
		AgentID:       agent,
		Model:         "claude-opus-4-7-mythos",
		ContentHash:   strings.Repeat("0", 64),
		SchemaVersion: 1,
	}

	signed := base
	if _, err := receipt.Sign(&signed, ks, time.Unix(1_700_000_001, 0)); err != nil {
		t.Fatalf("sign: %v", err)
	}

	t.Run("valid", func(t *testing.T) {
		v := signed
		verdict, code := verifyVerdict(&v, ks)
		if verdict != "valid" || code != rcExitValid {
			t.Fatalf("got (%q,%d), want (valid,%d)", verdict, code, rcExitValid)
		}
	})

	t.Run("invalid_signature", func(t *testing.T) {
		v := signed
		v.ContentHash = strings.Repeat("9", 64) // tamper a signed field
		verdict, code := verifyVerdict(&v, ks)
		if verdict != "invalid_signature" || code != rcExitInvalidSig {
			t.Fatalf("got (%q,%d), want (invalid_signature,%d)", verdict, code, rcExitInvalidSig)
		}
	})

	t.Run("unsupported_schema", func(t *testing.T) {
		v := signed
		v.SchemaVersion = 99
		verdict, code := verifyVerdict(&v, ks)
		if verdict != "unsupported_schema" || code != rcExitUnsupported {
			t.Fatalf("got (%q,%d), want (unsupported_schema,%d)", verdict, code, rcExitUnsupported)
		}
	})

	t.Run("unknown_key", func(t *testing.T) {
		v := signed
		empty := receipt.NewMemKeyStore() // no key for this agent's key_id
		verdict, code := verifyVerdict(&v, empty)
		if verdict != "unknown_key" || code != rcExitUnknownKey {
			t.Fatalf("got (%q,%d), want (unknown_key,%d)", verdict, code, rcExitUnknownKey)
		}
	})
}
