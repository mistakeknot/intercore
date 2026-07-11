package authz

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync"
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

func TestWriteKeyPair_RefusesPublicOnlyCheckout(t *testing.T) {
	root := t.TempDir()
	trusted, _ := GenerateKey()
	_, pubPath := KeyPaths(root)
	if err := os.MkdirAll(filepath.Dir(pubPath), 0o700); err != nil {
		t.Fatalf("mkdir keys: %v", err)
	}
	trustedHex := hexString(trusted.Pub) + "\n"
	if err := os.WriteFile(pubPath, []byte(trustedHex), 0o444); err != nil {
		t.Fatalf("write trusted pub: %v", err)
	}

	replacement, _ := GenerateKey()
	if err := WriteKeyPair(root, replacement); err != ErrKeyAlreadyExists {
		t.Fatalf("err = %v, want ErrKeyAlreadyExists", err)
	}
	got, err := os.ReadFile(pubPath)
	if err != nil {
		t.Fatalf("read trusted pub: %v", err)
	}
	if string(got) != trustedHex {
		t.Fatal("public-only checkout was overwritten")
	}
	privPath, _ := KeyPaths(root)
	if _, err := os.Stat(privPath); !os.IsNotExist(err) {
		t.Fatalf("private key unexpectedly created: %v", err)
	}
}

func TestWriteKeyPair_RefusesDanglingPrivateSymlink(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "escaped-private-key")
	privPath, _ := KeyPaths(root)
	if err := os.MkdirAll(filepath.Dir(privPath), 0o700); err != nil {
		t.Fatalf("mkdir keys: %v", err)
	}
	if err := os.Symlink(outside, privPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	kp, _ := GenerateKey()
	if err := WriteKeyPair(root, kp); err != ErrKeyAlreadyExists {
		t.Fatalf("err = %v, want ErrKeyAlreadyExists", err)
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("private key escaped project root: %v", err)
	}
}

func TestWriteKeyPair_RefusesSymlinkedClavainDirectory(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, ".clavain")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	kp, _ := GenerateKey()
	if err := WriteKeyPair(root, kp); err == nil {
		t.Fatal("WriteKeyPair should reject a symlinked .clavain directory")
	}
	if _, err := os.Stat(filepath.Join(outside, "keys", "authz-project.key")); !os.IsNotExist(err) {
		t.Fatalf("private key escaped project root: %v", err)
	}
}

func TestWriteKeyPair_ConcurrentInitHasSingleWinner(t *testing.T) {
	root := t.TempDir()
	kp1, _ := GenerateKey()
	kp2, _ := GenerateKey()
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, kp := range []KeyPair{kp1, kp2} {
		wg.Add(1)
		go func(kp KeyPair) {
			defer wg.Done()
			errs <- WriteKeyPair(root, kp)
		}(kp)
	}
	wg.Wait()
	close(errs)
	wins, exists := 0, 0
	for err := range errs {
		switch err {
		case nil:
			wins++
		default:
			if errors.Is(err, ErrKeyAlreadyExists) {
				exists++
				continue
			}
			t.Fatalf("unexpected concurrent init error: %v", err)
		}
	}
	if wins != 1 || exists != 1 {
		t.Fatalf("wins=%d already-exists=%d, want 1/1", wins, exists)
	}
	if _, err := LoadPrivKey(root); err != nil {
		t.Fatalf("winning keypair is inconsistent: %v", err)
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

func TestLoadPrivKey_RequiresPublicAnchor(t *testing.T) {
	root := t.TempDir()
	kp, _ := GenerateKey()
	if err := WriteKeyPair(root, kp); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, pubPath := KeyPaths(root)
	if err := os.Remove(pubPath); err != nil {
		t.Fatalf("remove public key: %v", err)
	}
	if _, err := LoadPrivKey(root); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("LoadPrivKey err=%v, want ErrKeyNotFound", err)
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

func TestLoadPrivKey_RejectsSeedTailInconsistency(t *testing.T) {
	root := t.TempDir()
	kp, _ := GenerateKey()
	if err := WriteKeyPair(root, kp); err != nil {
		t.Fatalf("write: %v", err)
	}
	privPath, _ := KeyPaths(root)
	data, err := os.ReadFile(privPath)
	if err != nil {
		t.Fatalf("read private key: %v", err)
	}
	privBytes, err := hex.DecodeString(string(data[:len(data)-1]))
	if err != nil {
		t.Fatalf("decode private key: %v", err)
	}
	privBytes[0] ^= 0xff
	if err := os.Chmod(privPath, 0o600); err != nil {
		t.Fatalf("chmod private key: %v", err)
	}
	if err := os.WriteFile(privPath, []byte(hex.EncodeToString(privBytes)+"\n"), 0o400); err != nil {
		t.Fatalf("rewrite private key: %v", err)
	}
	if err := os.Chmod(privPath, 0o400); err != nil {
		t.Fatalf("restore private mode: %v", err)
	}
	if _, err := LoadPrivKey(root); !errors.Is(err, ErrKeyCorrupted) {
		t.Fatalf("LoadPrivKey err=%v, want ErrKeyCorrupted", err)
	}
}

func TestKeyLoads_RejectSymlinkedKeysDirectory(t *testing.T) {
	trustedRoot := t.TempDir()
	kp, _ := GenerateKey()
	if err := WriteKeyPair(trustedRoot, kp); err != nil {
		t.Fatalf("write trusted pair: %v", err)
	}

	root := t.TempDir()
	clavainDir := filepath.Join(root, ".clavain")
	if err := os.Mkdir(clavainDir, 0o700); err != nil {
		t.Fatalf("mkdir .clavain: %v", err)
	}
	if err := os.Symlink(filepath.Join(trustedRoot, ".clavain", "keys"), filepath.Join(clavainDir, "keys")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := LoadPrivKey(root); !errors.Is(err, ErrKeyCorrupted) {
		t.Fatalf("LoadPrivKey err=%v, want ErrKeyCorrupted", err)
	}
	if _, err := LoadPubKey(root); !errors.Is(err, ErrKeyCorrupted) {
		t.Fatalf("LoadPubKey err=%v, want ErrKeyCorrupted", err)
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
