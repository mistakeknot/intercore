package authz

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// KeyPair holds a project authz signing keypair.
//
// The private key is used by `clavain-cli policy sign` and the matching
// public key by `policy audit --verify`. Rotation produces a new pair and
// archives the old one; the `sig_version` column in authorizations ties a
// row to the key it was signed with.
//
// See docs/canon/authz-signing-trust-model.md for the trust claim.
type KeyPair struct {
	Priv ed25519.PrivateKey
	Pub  ed25519.PublicKey
}

// Errors returned by the key package.
var (
	ErrKeyNotFound       = errors.New("authz: key not found")
	ErrKeyAlreadyExists  = errors.New("authz: key already exists (use --rotate to replace)")
	ErrKeyPermsTooBroad  = errors.New("authz: private key file permissions too broad (must be 0400)")
	ErrKeyCorrupted      = errors.New("authz: key file corrupted")
	ErrPubKeyMismatch    = errors.New("authz: public key does not match private key on disk")
)

// GenerateKey returns a fresh Ed25519 keypair.
func GenerateKey() (KeyPair, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return KeyPair{}, fmt.Errorf("generate ed25519: %w", err)
	}
	return KeyPair{Priv: priv, Pub: pub}, nil
}

// KeyPaths returns the conventional on-disk paths for the project keypair
// under projectRoot. The containing directory (.clavain/keys) is NOT
// created; WriteKeyPair handles that.
func KeyPaths(projectRoot string) (priv, pub string) {
	base := filepath.Join(projectRoot, ".clavain", "keys")
	return filepath.Join(base, "authz-project.key"), filepath.Join(base, "authz-project.pub")
}

// WriteKeyPair writes the keypair to disk with strict permissions:
//   - directory: 0700
//   - private key: 0400
//   - public key:  0444
//
// It refuses to overwrite an existing private key (the caller must archive
// the old one via rotation). The public key is overwritable because
// republishing a pubkey that was already on disk is a no-op and because
// rotation needs to land the new pub file atomically.
func WriteKeyPair(projectRoot string, kp KeyPair) error {
	privPath, pubPath := KeyPaths(projectRoot)
	dir := filepath.Dir(privPath)

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir keys dir: %w", err)
	}

	if _, err := os.Stat(privPath); err == nil {
		return ErrKeyAlreadyExists
	}

	privHex := hex.EncodeToString(kp.Priv) + "\n"
	if err := os.WriteFile(privPath, []byte(privHex), 0o400); err != nil {
		return fmt.Errorf("write private key: %w", err)
	}

	pubHex := hex.EncodeToString(kp.Pub) + "\n"
	if err := os.WriteFile(pubPath, []byte(pubHex), 0o444); err != nil {
		return fmt.Errorf("write public key: %w", err)
	}
	return nil
}

// LoadPrivKey reads the private keypair from projectRoot. It enforces the
// 0400 permissions requirement: a key file readable or writable by anyone
// other than the owner is rejected. This catches the common mistake of
// `chmod 644 authz-project.key` via a misapplied recursive permission fix.
//
// The file stores hex(ed25519.PrivateKey)+"\n"; we load both halves by
// rederiving the public key from the private (ed25519.PrivateKey embeds
// the public half in its seed-expanded form).
func LoadPrivKey(projectRoot string) (KeyPair, error) {
	privPath, pubPath := KeyPaths(projectRoot)

	info, err := os.Stat(privPath)
	if err != nil {
		if os.IsNotExist(err) {
			return KeyPair{}, ErrKeyNotFound
		}
		return KeyPair{}, fmt.Errorf("stat private key: %w", err)
	}
	if info.Mode().Perm() != 0o400 {
		return KeyPair{}, fmt.Errorf("%w: %s has mode %o", ErrKeyPermsTooBroad, privPath, info.Mode().Perm())
	}

	data, err := os.ReadFile(privPath)
	if err != nil {
		return KeyPair{}, fmt.Errorf("read private key: %w", err)
	}
	privBytes, err := hex.DecodeString(trimTrailingNewline(string(data)))
	if err != nil {
		return KeyPair{}, fmt.Errorf("%w: %v", ErrKeyCorrupted, err)
	}
	if len(privBytes) != ed25519.PrivateKeySize {
		return KeyPair{}, fmt.Errorf("%w: private key length %d, want %d", ErrKeyCorrupted, len(privBytes), ed25519.PrivateKeySize)
	}

	priv := ed25519.PrivateKey(privBytes)
	pubFromPriv, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return KeyPair{}, fmt.Errorf("%w: cannot derive public key", ErrKeyCorrupted)
	}

	// Cross-check against the stored pub file when present so we detect
	// the case where an attacker swapped only one half.
	if diskPub, err := loadPubFile(pubPath); err == nil {
		if !keysEqual(diskPub, pubFromPriv) {
			return KeyPair{}, ErrPubKeyMismatch
		}
	}

	return KeyPair{Priv: priv, Pub: pubFromPriv}, nil
}

// LoadPubKey reads only the public key (no permission check, no private
// file required). Callers that only need to verify signatures use this.
func LoadPubKey(projectRoot string) (ed25519.PublicKey, error) {
	_, pubPath := KeyPaths(projectRoot)
	return loadPubFile(pubPath)
}

// KeyFingerprint returns a short stable identifier for a public key:
// first 8 bytes of sha256(pub) as lowercase hex. Used in audit output and
// key-rotation archive filenames.
func KeyFingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// RotateKey archives the current private and public keys under fingerprint-
// suffixed names, then writes the provided new keypair at the canonical
// paths. Returns the archived private path and public path for logging.
// The caller is responsible for generating the new keypair.
func RotateKey(projectRoot string, newKP KeyPair) (archivedPriv, archivedPub string, err error) {
	privPath, pubPath := KeyPaths(projectRoot)

	existing, err := LoadPrivKey(projectRoot)
	if err != nil {
		return "", "", fmt.Errorf("load existing key for rotation: %w", err)
	}
	fp := KeyFingerprint(existing.Pub)

	archivedPriv = privPath + "." + fp + ".archive"
	archivedPub = pubPath + "." + fp

	// Temporarily relax perms on the existing private file so the rename
	// doesn't fail on a read-only 0400 parent-dir quirk on some filesystems.
	// (On POSIX, rename doesn't require write on the source file — but this
	// is defensive against quirky FUSE/NFS setups.)
	if err := os.Chmod(privPath, 0o600); err != nil {
		return "", "", fmt.Errorf("pre-rotate chmod: %w", err)
	}
	if err := os.Rename(privPath, archivedPriv); err != nil {
		return "", "", fmt.Errorf("archive private key: %w", err)
	}
	if err := os.Chmod(archivedPriv, 0o400); err != nil {
		return "", "", fmt.Errorf("post-rotate chmod archive: %w", err)
	}
	if err := os.Rename(pubPath, archivedPub); err != nil {
		return "", "", fmt.Errorf("archive public key: %w", err)
	}

	if err := WriteKeyPair(projectRoot, newKP); err != nil {
		return "", "", fmt.Errorf("write new key: %w", err)
	}
	return archivedPriv, archivedPub, nil
}

// loadPubFile reads and parses a public-key file, with no permission check.
func loadPubFile(path string) (ed25519.PublicKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrKeyNotFound
		}
		return nil, fmt.Errorf("read pub key %s: %w", path, err)
	}
	pubBytes, err := hex.DecodeString(trimTrailingNewline(string(data)))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrKeyCorrupted, err)
	}
	if len(pubBytes) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%w: public key length %d, want %d", ErrKeyCorrupted, len(pubBytes), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(pubBytes), nil
}

func trimTrailingNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r' || s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

func keysEqual(a, b ed25519.PublicKey) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
