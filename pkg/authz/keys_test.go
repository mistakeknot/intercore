package authz

import (
	"crypto/ed25519"
	"os"
	"path/filepath"
	"testing"
)

func TestGenerateKey_UniquePairs(t *testing.T) {
	kp1, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey 1: %v", err)
	}
	kp2, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey 2: %v", err)
	}
	if string(kp1.Priv) == string(kp2.Priv) {
		t.Fatal("two generated private keys are identical — entropy failure")
	}
	// Public key must round-trip from private.
	if string(kp1.Pub) != string(kp1.Priv.Public().(ed25519.PublicKey)) {
		t.Fatal("public key does not round-trip from private key")
	}
}

func TestWriteKeyPair_CorrectPerms(t *testing.T) {
	root := t.TempDir()
	kp, _ := GenerateKey()
	if err := WriteKeyPair(root, kp); err != nil {
		t.Fatalf("WriteKeyPair: %v", err)
	}

	privPath, pubPath := KeyPaths(root)
	privInfo, err := os.Stat(privPath)
	if err != nil {
		t.Fatalf("stat private: %v", err)
	}
	if privInfo.Mode().Perm() != 0o400 {
		t.Errorf("private key perms = %o, want 0400", privInfo.Mode().Perm())
	}

	pubInfo, err := os.Stat(pubPath)
	if err != nil {
		t.Fatalf("stat public: %v", err)
	}
	if pubInfo.Mode().Perm() != 0o444 {
		t.Errorf("public key perms = %o, want 0444", pubInfo.Mode().Perm())
	}

	dirInfo, err := os.Stat(filepath.Dir(privPath))
	if err != nil {
		t.Fatalf("stat keys dir: %v", err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Errorf("keys dir perms = %o, want 0700", dirInfo.Mode().Perm())
	}
}

func TestWriteKeyPair_RefusesOverwrite(t *testing.T) {
	root := t.TempDir()
	kp1, _ := GenerateKey()
	if err := WriteKeyPair(root, kp1); err != nil {
		t.Fatalf("first write: %v", err)
	}

	kp2, _ := GenerateKey()
	err := WriteKeyPair(root, kp2)
	if err == nil {
		t.Fatal("WriteKeyPair should refuse to overwrite existing private key")
	}
	if err != ErrKeyAlreadyExists {
		t.Errorf("err = %v, want ErrKeyAlreadyExists", err)
	}
}

func TestLoadPrivKey_RoundTrip(t *testing.T) {
	root := t.TempDir()
	kp, _ := GenerateKey()
	if err := WriteKeyPair(root, kp); err != nil {
		t.Fatalf("write: %v", err)
	}
	loaded, err := LoadPrivKey(root)
	if err != nil {
		t.Fatalf("LoadPrivKey: %v", err)
	}
	if string(loaded.Priv) != string(kp.Priv) {
		t.Fatal("loaded private key does not match written")
	}
	if string(loaded.Pub) != string(kp.Pub) {
		t.Fatal("loaded public key does not match written")
	}
}

func TestLoadPrivKey_RejectsTooPermissive(t *testing.T) {
	root := t.TempDir()
	kp, _ := GenerateKey()
	if err := WriteKeyPair(root, kp); err != nil {
		t.Fatalf("write: %v", err)
	}
	privPath, _ := KeyPaths(root)
	if err := os.Chmod(privPath, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	_, err := LoadPrivKey(root)
	if err == nil {
		t.Fatal("LoadPrivKey should reject 0644 private key")
	}
	if !errorIs(err, ErrKeyPermsTooBroad) {
		t.Errorf("err = %v, want contains ErrKeyPermsTooBroad", err)
	}
}

func TestLoadPrivKey_MissingReturnsSentinel(t *testing.T) {
	root := t.TempDir()
	_, err := LoadPrivKey(root)
	if err != ErrKeyNotFound {
		t.Errorf("err = %v, want ErrKeyNotFound", err)
	}
}

func TestLoadPrivKey_PubKeyMismatchDetected(t *testing.T) {
	root := t.TempDir()
	kp1, _ := GenerateKey()
	if err := WriteKeyPair(root, kp1); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Overwrite public file with an unrelated key.
	_, pubPath := KeyPaths(root)
	if err := os.Chmod(pubPath, 0o644); err != nil {
		t.Fatalf("chmod pub: %v", err)
	}
	kp2, _ := GenerateKey()
	pubHex := hexString(kp2.Pub) + "\n"
	if err := os.WriteFile(pubPath, []byte(pubHex), 0o444); err != nil {
		t.Fatalf("overwrite pub: %v", err)
	}

	_, err := LoadPrivKey(root)
	if err != ErrPubKeyMismatch {
		t.Errorf("err = %v, want ErrPubKeyMismatch", err)
	}
}

func TestLoadPubKey_StandaloneLoad(t *testing.T) {
	root := t.TempDir()
	kp, _ := GenerateKey()
	if err := WriteKeyPair(root, kp); err != nil {
		t.Fatalf("write: %v", err)
	}
	pub, err := LoadPubKey(root)
	if err != nil {
		t.Fatalf("LoadPubKey: %v", err)
	}
	if string(pub) != string(kp.Pub) {
		t.Error("loaded public key does not match written")
	}
}

func TestKeyFingerprint_Stable(t *testing.T) {
	kp, _ := GenerateKey()
	fp1 := KeyFingerprint(kp.Pub)
	fp2 := KeyFingerprint(kp.Pub)
	if fp1 != fp2 {
		t.Errorf("fingerprint not deterministic: %q vs %q", fp1, fp2)
	}
	if len(fp1) != 16 { // 8 bytes hex = 16 chars
		t.Errorf("fingerprint len = %d, want 16", len(fp1))
	}
}

func TestRotateKey_ArchivesAndReplaces(t *testing.T) {
	root := t.TempDir()
	kp1, _ := GenerateKey()
	if err := WriteKeyPair(root, kp1); err != nil {
		t.Fatalf("initial write: %v", err)
	}
	fp1 := KeyFingerprint(kp1.Pub)

	kp2, _ := GenerateKey()
	archivedPriv, archivedPub, err := RotateKey(root, kp2)
	if err != nil {
		t.Fatalf("RotateKey: %v", err)
	}
	if _, err := os.Stat(archivedPriv); err != nil {
		t.Errorf("archived private not found: %v", err)
	}
	if _, err := os.Stat(archivedPub); err != nil {
		t.Errorf("archived public not found: %v", err)
	}

	privPath, _ := KeyPaths(root)
	info, err := os.Stat(privPath)
	if err != nil {
		t.Fatalf("new private file: %v", err)
	}
	if info.Mode().Perm() != 0o400 {
		t.Errorf("new private perms = %o, want 0400", info.Mode().Perm())
	}

	loaded, err := LoadPrivKey(root)
	if err != nil {
		t.Fatalf("LoadPrivKey after rotate: %v", err)
	}
	if string(loaded.Priv) != string(kp2.Priv) {
		t.Error("new private key does not match kp2")
	}

	// Archive filenames should embed the old fingerprint.
	if want := privPath + "." + fp1 + ".archive"; archivedPriv != want {
		t.Errorf("archivedPriv = %q, want %q", archivedPriv, want)
	}
}

// ─── helpers ─────────────────────────────────────────────────────────

func errorIs(err, target error) bool {
	for e := err; e != nil; {
		if e == target {
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := e.(unwrapper)
		if !ok {
			break
		}
		e = u.Unwrap()
	}
	// Also accept target-in-Error-string for wrapped %w forms.
	return false
}

func hexString(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = digits[v>>4]
		out[i*2+1] = digits[v&0xf]
	}
	return string(out)
}
