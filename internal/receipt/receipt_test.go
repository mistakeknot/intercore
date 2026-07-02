package receipt

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	mathrand "math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testKey is a fixed 32-byte signing key used by golden-vector tests. Pinning
// the key (rather than generating one per test run) makes HMAC outputs
// reproducible across machines and CI runs, which is the whole point of
// golden vectors.
var testKey = bytes.Repeat([]byte{0x42}, 32)

const testAgentID = "sylveste://agent/test"
const testEpoch = "2026-test"

// goldenStore returns a MemKeyStore preloaded with testKey under testAgentID
// at testEpoch. Use this in any test that needs deterministic signing.
func goldenStore() *MemKeyStore {
	s := NewMemKeyStore()
	s.Register(testAgentID, testEpoch, testKey)
	return s
}

// goldenCase pins one receipt + its expected canonical bytes + its expected
// HMAC hex. The HMAC is filled in by running the test once with
// RECEIPT_GOLDEN_UPDATE=1 and pasting the printed value; see
// TestGoldenVectors.
type goldenCase struct {
	name          string
	receipt       Receipt
	expectedCanon string
	expectedHMAC  string
}

func ptr(s string) *string { return &s }

func goldenVectors() []goldenCase {
	return []goldenCase{
		{
			name: "minimal-empty-tool-calls-null-parent",
			receipt: Receipt{
				ReceiptID:     "rcpt_01ARZ3NDEKTSV4RRFFQ69G5FAV",
				Timestamp:     "2026-05-23T19:42:01.234567Z",
				AgentID:       testAgentID,
				Model:         "claude-opus-4-7-mythos",
				ToolCalls:     nil,
				ParentRunID:   nil,
				ContentHash:   "0000000000000000000000000000000000000000000000000000000000000000",
				SchemaVersion: 1,
			},
			expectedCanon: `{"receipt_id":"rcpt_01ARZ3NDEKTSV4RRFFQ69G5FAV","timestamp":"2026-05-23T19:42:01.234567Z","agent_id":"sylveste://agent/test","model":"claude-opus-4-7-mythos","tool_calls":[],"parent_run_id":null,"content_hash":"0000000000000000000000000000000000000000000000000000000000000000","schema_version":1}`,
			expectedHMAC:  "e2b6dabfc2dc6de183e29b87cea03ff751bd1236d518ee2d1a0ab5d1a1c0d693",
		},
		{
			name: "one-tool-call-string-parent",
			receipt: Receipt{
				ReceiptID:   "rcpt_01ARZ3NDEKTSV4RRFFQ69G5FBW",
				Timestamp:   "2026-05-23T19:42:02.000000Z",
				AgentID:     testAgentID,
				Model:       "claude-sonnet-4-7",
				ParentRunID: ptr("task-abc-123"),
				ContentHash: "1111111111111111111111111111111111111111111111111111111111111111",
				ToolCalls: []ToolCall{
					{Name: "Bash", ArgsHash: "aa", ResultHash: "bb", DurationMs: 42},
				},
				SchemaVersion: 1,
			},
			expectedCanon: `{"receipt_id":"rcpt_01ARZ3NDEKTSV4RRFFQ69G5FBW","timestamp":"2026-05-23T19:42:02.000000Z","agent_id":"sylveste://agent/test","model":"claude-sonnet-4-7","tool_calls":[{"name":"Bash","args_hash":"aa","result_hash":"bb","duration_ms":42}],"parent_run_id":"task-abc-123","content_hash":"1111111111111111111111111111111111111111111111111111111111111111","schema_version":1}`,
			expectedHMAC:  "19755de07450a36d062d8d111154f6a0ea9125875c3236e58130da132af6dac9",
		},
		{
			name: "multi-tool-calls-ordered",
			receipt: Receipt{
				ReceiptID:   "rcpt_01ARZ3NDEKTSV4RRFFQ69G5FCX",
				Timestamp:   "2026-05-23T19:42:03.000000Z",
				AgentID:     testAgentID,
				Model:       "claude-opus-4-7-mythos",
				ContentHash: "2222222222222222222222222222222222222222222222222222222222222222",
				ToolCalls: []ToolCall{
					{Name: "Read", ArgsHash: "01", ResultHash: "02", DurationMs: 5},
					{Name: "Edit", ArgsHash: "03", ResultHash: "04", DurationMs: 12},
					{Name: "Bash", ArgsHash: "05", ResultHash: "06", DurationMs: 100},
				},
				SchemaVersion: 1,
			},
			expectedCanon: `{"receipt_id":"rcpt_01ARZ3NDEKTSV4RRFFQ69G5FCX","timestamp":"2026-05-23T19:42:03.000000Z","agent_id":"sylveste://agent/test","model":"claude-opus-4-7-mythos","tool_calls":[{"name":"Read","args_hash":"01","result_hash":"02","duration_ms":5},{"name":"Edit","args_hash":"03","result_hash":"04","duration_ms":12},{"name":"Bash","args_hash":"05","result_hash":"06","duration_ms":100}],"parent_run_id":null,"content_hash":"2222222222222222222222222222222222222222222222222222222222222222","schema_version":1}`,
			expectedHMAC:  "62cade2913ffe9ee0fababc157d03fda35941906e02b2455505c817e9c4c296d",
		},
		{
			name: "control-chars-and-quotes-in-strings",
			receipt: Receipt{
				ReceiptID:   "rcpt_01ARZ3NDEKTSV4RRFFQ69G5FDY",
				Timestamp:   "2026-05-23T19:42:04.000000Z",
				AgentID:     testAgentID,
				Model:       "model-with-\"quotes\"-and-\\backslash",
				ContentHash: "3333333333333333333333333333333333333333333333333333333333333333",
				ToolCalls: []ToolCall{
					{Name: "Write", ArgsHash: "tab\there\nnewline\rreturn", ResultHash: "ok", DurationMs: 1},
				},
				SchemaVersion: 1,
			},
			expectedCanon: `{"receipt_id":"rcpt_01ARZ3NDEKTSV4RRFFQ69G5FDY","timestamp":"2026-05-23T19:42:04.000000Z","agent_id":"sylveste://agent/test","model":"model-with-\"quotes\"-and-\\backslash","tool_calls":[{"name":"Write","args_hash":"tab\there\nnewline\rreturn","result_hash":"ok","duration_ms":1}],"parent_run_id":null,"content_hash":"3333333333333333333333333333333333333333333333333333333333333333","schema_version":1}`,
			expectedHMAC:  "bf00a0d0f7f0f5ee7f42509a964956ad9c77f525d8063a11c8d21fb64072b96c",
		},
		{
			name: "low-control-char-uXXXX-escape",
			receipt: Receipt{
				ReceiptID:   "rcpt_01ARZ3NDEKTSV4RRFFQ69G5FEZ",
				Timestamp:   "2026-05-23T19:42:05.000000Z",
				AgentID:     testAgentID,
				Model:       "control-\x01-char",
				ContentHash: "4444444444444444444444444444444444444444444444444444444444444444",
				ToolCalls: []ToolCall{
					{Name: "X", ArgsHash: "\x00", ResultHash: "\x1f", DurationMs: 0},
				},
				SchemaVersion: 1,
			},
			expectedCanon: `{"receipt_id":"rcpt_01ARZ3NDEKTSV4RRFFQ69G5FEZ","timestamp":"2026-05-23T19:42:05.000000Z","agent_id":"sylveste://agent/test","model":"control-\u0001-char","tool_calls":[{"name":"X","args_hash":"\u0000","result_hash":"\u001f","duration_ms":0}],"parent_run_id":null,"content_hash":"4444444444444444444444444444444444444444444444444444444444444444","schema_version":1}`,
			expectedHMAC:  "ab538d1083270fd55300999b568ff1bf64a9d97cf3846482d26e615559c13002",
		},
	}
}

// TestGoldenVectors locks the canonical byte stream and HMAC output for five
// hand-canonicalized receipts. This is the spec-anchor regression net per
// bead acceptance criterion 2 and canon §Canonicalization.
//
// Update workflow:
//
//   - Edit canonical-bytes for a case if (and only if) the canon spec changes.
//   - To re-seed HMAC values, run the test once: it will emit the computed
//     hex on failure for any case whose expectedHMAC starts with PLACEHOLDER_.
//     Paste the printed values back into goldenVectors().
func TestGoldenVectors(t *testing.T) {
	store := goldenStore()
	for _, c := range goldenVectors() {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got := string(Canonicalize(&c.receipt))
			if got != c.expectedCanon {
				t.Fatalf("canonical bytes mismatch\nwant: %s\ngot:  %s", c.expectedCanon, got)
			}

			// Compute HMAC fresh (do not go through Sign — Sign mutates the
			// receipt and we want the HMAC over the bytes we just locked).
			mac := hmac.New(sha256.New, testKey)
			mac.Write([]byte(c.expectedCanon))
			sig := hex.EncodeToString(mac.Sum(nil))

			if strings.HasPrefix(c.expectedHMAC, "PLACEHOLDER_") {
				t.Fatalf("expectedHMAC unset for %q — paste this into goldenVectors():\n\t\"%s\"", c.name, sig)
			}
			if sig != c.expectedHMAC {
				t.Fatalf("hmac mismatch\nwant: %s\ngot:  %s", c.expectedHMAC, sig)
			}

			// And confirm Sign produces the same value through the public API.
			signed := c.receipt
			canonFromSign, err := Sign(&signed, store, time.Unix(0, 0).UTC())
			if err != nil {
				t.Fatalf("Sign returned error: %v", err)
			}
			if string(canonFromSign) != c.expectedCanon {
				t.Fatalf("Sign() returned different canonical bytes\nwant: %s\ngot:  %s",
					c.expectedCanon, string(canonFromSign))
			}
			if signed.Signature != c.expectedHMAC {
				t.Fatalf("Sign() set Signature to %s, want %s", signed.Signature, c.expectedHMAC)
			}
			if signed.KeyID != testAgentID+"#"+testEpoch {
				t.Fatalf("Sign() set KeyID to %q, want %q",
					signed.KeyID, testAgentID+"#"+testEpoch)
			}
		})
	}
}

// TestRoundTrip exercises Sign → Verify across 100 randomised receipts,
// covering the property "any receipt that Sign accepts, Verify accepts".
//
// Random shapes include: 0/1/many tool_calls, nil vs string ParentRunID,
// random ASCII content with occasional escape-triggering bytes.
func TestRoundTrip(t *testing.T) {
	store := goldenStore()
	rng := mathrand.New(mathrand.NewSource(1))
	for i := 0; i < 100; i++ {
		r := randomReceipt(rng, i)
		original := r
		if _, err := Sign(&r, store, time.Now().UTC()); err != nil {
			t.Fatalf("iter %d: Sign error: %v", i, err)
		}
		if err := Verify(&r, store); err != nil {
			t.Fatalf("iter %d: Verify error: %v\n  receipt: %+v", i, err, original)
		}

		// Tamper with one signed field and confirm Verify rejects.
		tampered := r
		tampered.ContentHash = strings.Repeat("9", 64)
		if err := Verify(&tampered, store); !errors.Is(err, ErrInvalidSignature) {
			t.Fatalf("iter %d: tampered receipt verified (want ErrInvalidSignature, got %v)",
				i, err)
		}
	}
}

func randomReceipt(rng *mathrand.Rand, seed int) Receipt {
	nCalls := rng.Intn(4) // 0..3
	calls := make([]ToolCall, nCalls)
	for i := range calls {
		calls[i] = ToolCall{
			Name:       fmt.Sprintf("Tool%d", rng.Intn(10)),
			ArgsHash:   randHex(rng, 8),
			ResultHash: randHex(rng, 8),
			DurationMs: int64(rng.Intn(1000)),
		}
	}
	var parent *string
	if rng.Intn(2) == 1 {
		p := fmt.Sprintf("task-%d", rng.Intn(1000))
		parent = &p
	}
	return Receipt{
		ReceiptID:     fmt.Sprintf("rcpt_%026d", seed),
		Timestamp:     FormatTimestamp(time.Unix(int64(1700000000+seed), 0)),
		AgentID:       testAgentID,
		Model:         "claude-test",
		ToolCalls:     calls,
		ParentRunID:   parent,
		ContentHash:   randHex(rng, 64),
		SchemaVersion: 1,
	}
}

func randHex(rng *mathrand.Rand, n int) string {
	const hex = "0123456789abcdef"
	out := make([]byte, n)
	for i := range out {
		out[i] = hex[rng.Intn(16)]
	}
	return string(out)
}

// TestVerifyErrors confirms the sentinel error mapping per canon §Verification
// semantics. The verify CLI in sylveste-ewy3.5.3 will errors.Is over these
// to pick the right exit code.
func TestVerifyErrors(t *testing.T) {
	store := goldenStore()
	base := Receipt{
		ReceiptID:     "rcpt_01ARZ3NDEKTSV4RRFFQ69G5FAV",
		Timestamp:     "2026-05-23T19:42:01.234567Z",
		AgentID:       testAgentID,
		Model:         "m",
		ContentHash:   strings.Repeat("0", 64),
		SchemaVersion: 1,
	}
	if _, err := Sign(&base, store, time.Now().UTC()); err != nil {
		t.Fatalf("base sign: %v", err)
	}

	t.Run("invalid-signature", func(t *testing.T) {
		bad := base
		bad.Signature = strings.Repeat("a", 64) // valid hex, wrong value
		if err := Verify(&bad, store); !errors.Is(err, ErrInvalidSignature) {
			t.Fatalf("want ErrInvalidSignature, got %v", err)
		}
	})

	t.Run("malformed-signature-hex", func(t *testing.T) {
		bad := base
		bad.Signature = "not-hex-zz"
		if err := Verify(&bad, store); !errors.Is(err, ErrInvalidSignature) {
			t.Fatalf("want ErrInvalidSignature, got %v", err)
		}
	})

	t.Run("schema-version-unsupported", func(t *testing.T) {
		bad := base
		bad.SchemaVersion = 99
		if err := Verify(&bad, store); !errors.Is(err, ErrSchemaUnsupported) {
			t.Fatalf("want ErrSchemaUnsupported, got %v", err)
		}
	})

	t.Run("signature-alg-unsupported", func(t *testing.T) {
		bad := base
		bad.SignatureAlg = "ed25519-v2"
		if err := Verify(&bad, store); !errors.Is(err, ErrSchemaUnsupported) {
			t.Fatalf("want ErrSchemaUnsupported, got %v", err)
		}
	})

	t.Run("key-not-found", func(t *testing.T) {
		bad := base
		bad.KeyID = "sylveste://agent/unknown#2026-q9"
		if err := Verify(&bad, store); !errors.Is(err, ErrKeyNotFound) {
			t.Fatalf("want ErrKeyNotFound, got %v", err)
		}
	})
}

func TestFileKeyStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	mem := goldenStore()
	keyID, key, err := mem.Active(testAgentID)
	if err != nil {
		t.Fatalf("mem active: %v", err)
	}
	_, epoch, _ := strings.Cut(keyID, "#")
	seg := agentSegment(testAgentID)
	agentDir := filepath.Join(dir, seg)
	if err := os.MkdirAll(agentDir, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, epoch+".key"), key, 0600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "active"), []byte(epoch), 0600); err != nil {
		t.Fatalf("write active: %v", err)
	}

	fs := &FileKeyStore{Root: dir}
	gotID, gotKey, err := fs.Active(testAgentID)
	if err != nil {
		t.Fatalf("file active: %v", err)
	}
	if gotID != keyID {
		t.Fatalf("active id: got %s want %s", gotID, keyID)
	}
	if !bytes.Equal(gotKey, key) {
		t.Fatal("active key bytes mismatch")
	}

	gotKey2, err := fs.Get(keyID)
	if err != nil {
		t.Fatalf("file get: %v", err)
	}
	if !bytes.Equal(gotKey2, key) {
		t.Fatal("get key bytes mismatch")
	}

	if _, err := fs.Get("nonexistent#x"); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("want ErrUnknownKey for missing key, got %v", err)
	}
}

func TestNewIDFormat(t *testing.T) {
	id := NewID(time.Now())
	if !strings.HasPrefix(id, IDPrefix) {
		t.Fatalf("id %q lacks prefix %q", id, IDPrefix)
	}
	if len(id) != len(IDPrefix)+26 {
		t.Fatalf("id length %d, want %d", len(id), len(IDPrefix)+26)
	}
}

func TestGenerateFileKey(t *testing.T) {
	root := t.TempDir()
	const agent = "sylveste://agent/interspect"

	keyID, err := GenerateFileKey(root, agent, "2026-q2", false)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if keyID != agent+"#2026-q2" {
		t.Fatalf("keyID = %q, want %q", keyID, agent+"#2026-q2")
	}

	// The generated key round-trips through FileKeyStore (the active pointer
	// and the key file are both written and readable).
	fs := &FileKeyStore{Root: root}
	gotID, gotKey, err := fs.Active(agent)
	if err != nil {
		t.Fatalf("active after generate: %v", err)
	}
	if gotID != keyID || len(gotKey) != 32 {
		t.Fatalf("active = (%q, %d bytes), want (%q, 32 bytes)", gotID, len(gotKey), keyID)
	}

	// A signed receipt verifies against the file-provisioned key — the whole
	// point of provisioning.
	r := Receipt{
		ReceiptID: NewID(time.Now()), Timestamp: FormatTimestamp(time.Now()),
		AgentID: agent, Model: "m", ContentHash: strings.Repeat("0", 64), SchemaVersion: 1,
	}
	if _, err := Sign(&r, fs, time.Now()); err != nil {
		t.Fatalf("sign with file key: %v", err)
	}
	if err := Verify(&r, fs); err != nil {
		t.Fatalf("verify with file key: %v", err)
	}

	// Refuses to overwrite an existing epoch without force.
	if _, err := GenerateFileKey(root, agent, "2026-q2", false); err == nil {
		t.Fatal("expected refusal to overwrite existing epoch, got nil")
	}
	if _, err := GenerateFileKey(root, agent, "2026-q2", true); err != nil {
		t.Fatalf("force overwrite should succeed: %v", err)
	}
}

func TestEnsureFileKey(t *testing.T) {
	root := t.TempDir()
	const agent = "sylveste://agent/clavain"

	id1, created1, err := EnsureFileKey(root, agent, "2026-q2")
	if err != nil || !created1 {
		t.Fatalf("first EnsureFileKey: id=%q created=%v err=%v (want created=true)", id1, created1, err)
	}
	id2, created2, err := EnsureFileKey(root, agent, "2026-q3")
	if err != nil {
		t.Fatalf("second EnsureFileKey: %v", err)
	}
	if created2 {
		t.Fatal("second EnsureFileKey created a new key; should have returned the existing active key")
	}
	if id2 != id1 {
		t.Fatalf("second EnsureFileKey returned %q, want existing %q", id2, id1)
	}
}

func TestFormatTimestampMicroseconds(t *testing.T) {
	got := FormatTimestamp(time.Date(2026, 5, 23, 19, 42, 1, 234_567_000, time.UTC))
	want := "2026-05-23T19:42:01.234567Z"
	if got != want {
		t.Fatalf("FormatTimestamp got %q want %q", got, want)
	}
}

// BenchmarkSign measures wall-time per Sign call. Canon acceptance criterion
// #4 budgets <5ms per receipt; this benchmark surfaces regressions early.
func BenchmarkSign(b *testing.B) {
	store := goldenStore()
	base := Receipt{
		AgentID:       testAgentID,
		Model:         "claude-opus-4-7-mythos",
		ContentHash:   strings.Repeat("ab", 32),
		SchemaVersion: 1,
		ToolCalls: []ToolCall{
			{Name: "Bash", ArgsHash: "cd", ResultHash: "ef", DurationMs: 7},
		},
	}
	rid := make([]byte, 16)
	_, _ = rand.Read(rid)
	base.ReceiptID = "rcpt_" + hex.EncodeToString(rid)[:26]
	base.Timestamp = FormatTimestamp(time.Now())
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := base
		if _, err := Sign(&r, store, time.Now()); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkVerify measures wall-time per Verify call. Canon budget: <10ms.
func BenchmarkVerify(b *testing.B) {
	store := goldenStore()
	r := Receipt{
		AgentID:       testAgentID,
		Model:         "claude-opus-4-7-mythos",
		ContentHash:   strings.Repeat("ab", 32),
		SchemaVersion: 1,
		ToolCalls: []ToolCall{
			{Name: "Bash", ArgsHash: "cd", ResultHash: "ef", DurationMs: 7},
		},
		ReceiptID: "rcpt_01ARZ3NDEKTSV4RRFFQ69G5FAV",
		Timestamp: "2026-05-23T19:42:01.234567Z",
	}
	if _, err := Sign(&r, store, time.Now()); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := Verify(&r, store); err != nil {
			b.Fatal(err)
		}
	}
}
