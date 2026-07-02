package authz

import (
	"testing"
)

// TestCanonicalPayload_Example1 matches Example 1 from
// docs/canon/authz-signing-payload.md: all fields populated.
func TestCanonicalPayload_Example1(t *testing.T) {
	row := SignRow{
		ID:             "01HQ8YR7JCMV7K8WK5T6V9BGQF",
		OpType:         "bead-close",
		Target:         "sylveste-qdqr",
		AgentID:        "claude-opus-4-7",
		BeadID:         "sylveste-qdqr",
		Mode:           "auto",
		PolicyMatch:    "bead-close#0",
		PolicyHash:     "f3f77555ffc398ff8af8e63f8518e3d9d6764fc7e487dfb9b3999755ccf10340",
		VettedSHA:      "0a1e85a6f9b7119988109b796dd2ca14f46b28c9",
		Vetting:        `{"shas":{"intercore":"0a1e85a"}}`,
		CrossProjectID: "",
		CreatedAt:      1776616956,
	}
	payload, err := CanonicalPayload(row)
	if err != nil {
		t.Fatalf("CanonicalPayload: %v", err)
	}
	want := "01HQ8YR7JCMV7K8WK5T6V9BGQF\n" +
		"bead-close\n" +
		"sylveste-qdqr\n" +
		"claude-opus-4-7\n" +
		"sylveste-qdqr\n" +
		"auto\n" +
		"bead-close#0\n" +
		"f3f77555ffc398ff8af8e63f8518e3d9d6764fc7e487dfb9b3999755ccf10340\n" +
		"0a1e85a6f9b7119988109b796dd2ca14f46b28c9\n" +
		`{"shas":{"intercore":"0a1e85a"}}` + "\n" +
		"\n" +
		"1776616956"
	if string(payload) != want {
		t.Errorf("payload mismatch\n got: %q\nwant: %q", string(payload), want)
	}
}

// TestCanonicalPayload_Example2: optional fields NULL.
func TestCanonicalPayload_Example2(t *testing.T) {
	row := SignRow{
		ID:        "01HQ8YRDABDCEFGHJKMNPQRSTV",
		OpType:    "git-push-main",
		Target:    "origin/main",
		AgentID:   "claude-opus-4-7",
		Mode:      "confirmed",
		PolicyMatch: "git-push-main#1",
		PolicyHash:  "9b2a...",
		CreatedAt: 1776617000,
	}
	payload, err := CanonicalPayload(row)
	if err != nil {
		t.Fatalf("CanonicalPayload: %v", err)
	}
	want := "01HQ8YRDABDCEFGHJKMNPQRSTV\n" +
		"git-push-main\n" +
		"origin/main\n" +
		"claude-opus-4-7\n" +
		"\n" +
		"confirmed\n" +
		"git-push-main#1\n" +
		"9b2a...\n" +
		"\n" +
		"\n" +
		"\n" +
		"1776617000"
	if string(payload) != want {
		t.Errorf("payload mismatch\n got: %q\nwant: %q", string(payload), want)
	}
}

// TestCanonicalPayload_Example3: cutover marker row.
func TestCanonicalPayload_Example3(t *testing.T) {
	row := SignRow{
		ID:        "01HQ8YSAAAAAAAAAAAAAAAAAAA",
		OpType:    "migration.signing-enabled",
		Target:    "authorizations",
		AgentID:   "system:migration-033",
		Mode:      "auto",
		CreatedAt: 1776618000,
	}
	payload, err := CanonicalPayload(row)
	if err != nil {
		t.Fatalf("CanonicalPayload: %v", err)
	}
	want := "01HQ8YSAAAAAAAAAAAAAAAAAAA\n" +
		"migration.signing-enabled\n" +
		"authorizations\n" +
		"system:migration-033\n" +
		"\n" +
		"auto\n" +
		"\n" +
		"\n" +
		"\n" +
		"\n" +
		"\n" +
		"1776618000"
	if string(payload) != want {
		t.Errorf("payload mismatch\n got: %q\nwant: %q", string(payload), want)
	}
}

// TestCanonicalPayload_RejectsControlChar proves the signer refuses to
// silently transliterate \r or non-LF control characters.
func TestCanonicalPayload_RejectsControlChar(t *testing.T) {
	row := SignRow{
		ID:        "a",
		OpType:    "bead-close",
		Target:    "x\ry", // CR is forbidden
		AgentID:   "test",
		Mode:      "auto",
		CreatedAt: 1,
	}
	if _, err := CanonicalPayload(row); err == nil {
		t.Fatal("expected CanonicalPayload to reject CR in a text field")
	}
}

func TestCanonicalPayload_RejectsNegativeCreatedAt(t *testing.T) {
	row := SignRow{
		ID:        "a",
		OpType:    "bead-close",
		Target:    "x",
		AgentID:   "t",
		Mode:      "auto",
		CreatedAt: -1,
	}
	if _, err := CanonicalPayload(row); err == nil {
		t.Fatal("expected CanonicalPayload to reject negative created_at")
	}
}

// TestSign_RoundTrip covers the happy path.
func TestSign_RoundTrip(t *testing.T) {
	kp, _ := GenerateKey()
	row := SignRow{
		ID:        "roundtrip-1",
		OpType:    "bead-close",
		Target:    "sylveste-x",
		AgentID:   "agent-1",
		Mode:      "auto",
		CreatedAt: 1000,
	}
	sig, err := Sign(kp.Priv, row)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if !Verify(kp.Pub, row, sig) {
		t.Fatal("Verify: signature should verify on the same row")
	}
}

// TestVerify_DetectsMutation: changing any signed field after signing
// must cause Verify to return false.
func TestVerify_DetectsMutation(t *testing.T) {
	kp, _ := GenerateKey()
	row := SignRow{
		ID:        "mut-1",
		OpType:    "bead-close",
		Target:    "sylveste-x",
		AgentID:   "agent-1",
		Mode:      "auto",
		CreatedAt: 1000,
	}
	sig, err := Sign(kp.Priv, row)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*SignRow)
	}{
		{"id", func(r *SignRow) { r.ID = "mut-2" }},
		{"op_type", func(r *SignRow) { r.OpType = "git-push-main" }},
		{"target", func(r *SignRow) { r.Target = "sylveste-y" }},
		{"agent_id", func(r *SignRow) { r.AgentID = "agent-2" }},
		{"mode", func(r *SignRow) { r.Mode = "confirmed" }},
		{"created_at", func(r *SignRow) { r.CreatedAt = 1001 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mut := row
			tc.mutate(&mut)
			if Verify(kp.Pub, mut, sig) {
				t.Errorf("Verify should fail after mutating %s", tc.name)
			}
		})
	}
}

// TestVerify_WrongKey: a sig made with one key must not verify under another.
func TestVerify_WrongKey(t *testing.T) {
	kpA, _ := GenerateKey()
	kpB, _ := GenerateKey()
	row := SignRow{
		ID: "x", OpType: "bead-close", Target: "t", AgentID: "a",
		Mode: "auto", CreatedAt: 1,
	}
	sig, _ := Sign(kpA.Priv, row)
	if Verify(kpB.Pub, row, sig) {
		t.Fatal("sig made with key A must not verify under key B")
	}
}

// TestVerify_WrongLengthSig returns false without panicking.
func TestVerify_WrongLengthSig(t *testing.T) {
	kp, _ := GenerateKey()
	row := SignRow{
		ID: "x", OpType: "bead-close", Target: "t", AgentID: "a",
		Mode: "auto", CreatedAt: 1,
	}
	if Verify(kp.Pub, row, []byte{1, 2, 3}) {
		t.Fatal("sig of wrong length must not verify")
	}
}

// TestCanonicalPayload_GoldenFixtures cross-checks every example in the
// canon doc produces output the reader-level test expects. One place for
// every golden fixture so doc + implementation stay in sync.
func TestCanonicalPayload_GoldenFixtures(t *testing.T) {
	t.Run("example1", TestCanonicalPayload_Example1)
	t.Run("example2", TestCanonicalPayload_Example2)
	t.Run("example3", TestCanonicalPayload_Example3)
}
