package authz

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
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
// archives the old one. Callers must not rotate retained signed history until
// verification supports multiple signer identities.
//
// See docs/canon/authz-signing-trust-model.md for the trust claim.
type KeyPair struct {
	Priv ed25519.PrivateKey
	Pub  ed25519.PublicKey
}

// Errors returned by the key package.
var (
	ErrKeyNotFound      = errors.New("authz: key not found")
	ErrKeyAlreadyExists = errors.New("authz: key already exists (use --rotate to replace)")
	ErrKeyPermsTooBroad = errors.New("authz: private key file permissions too broad (must be 0400)")
	ErrKeyCorrupted     = errors.New("authz: key file corrupted")
	ErrPubKeyMismatch   = errors.New("authz: public key does not match private key on disk")
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
// It refuses to overwrite either half of an existing keypair. A public-only
// checkout is a verifier, not permission to mint a new private identity and
// replace the trusted public key. Rotation archives both halves first.
func WriteKeyPair(projectRoot string, kp KeyPair) error {
	if err := validateKeyPair(kp); err != nil {
		return err
	}
	privPath, pubPath := KeyPaths(projectRoot)
	dir := filepath.Dir(privPath)
	clavainDir := filepath.Dir(dir)

	if err := ensureRealDirectory(clavainDir, false); err != nil {
		return fmt.Errorf("prepare .clavain dir: %w", err)
	}
	if err := ensureRealDirectory(dir, true); err != nil {
		return fmt.Errorf("prepare keys dir: %w", err)
	}

	for _, path := range []string{privPath, pubPath} {
		if _, err := os.Lstat(path); err == nil {
			return ErrKeyAlreadyExists
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("lstat key path %s: %w", path, err)
		}
	}

	privHex := hex.EncodeToString(kp.Priv) + "\n"
	if err := writeExclusiveKeyFile(privPath, []byte(privHex), 0o400); err != nil {
		return fmt.Errorf("write private key: %w", err)
	}

	pubHex := hex.EncodeToString(kp.Pub) + "\n"
	if err := writeExclusiveKeyFile(pubPath, []byte(pubHex), 0o444); err != nil {
		_ = os.Remove(privPath)
		return fmt.Errorf("write public key: %w", err)
	}
	return nil
}

func ensureRealDirectory(path string, enforcePrivateMode bool) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		if mkdirErr := os.Mkdir(path, 0o700); mkdirErr != nil && !os.IsExist(mkdirErr) {
			return mkdirErr
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("path is not a real directory: %s", path)
	}
	if enforcePrivateMode {
		if err := os.Chmod(path, 0o700); err != nil {
			return err
		}
	}
	return nil
}

func validateKeyDirectories(projectRoot string) error {
	_, pubPath := KeyPaths(projectRoot)
	for _, path := range []string{filepath.Dir(filepath.Dir(pubPath)), filepath.Dir(pubPath)} {
		info, err := os.Lstat(path)
		if err != nil {
			if os.IsNotExist(err) {
				return ErrKeyNotFound
			}
			return fmt.Errorf("lstat key directory %s: %w", path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("%w: key directory is not a real directory: %s", ErrKeyCorrupted, path)
		}
	}
	return nil
}

func validatePrivateKey(priv ed25519.PrivateKey) (ed25519.PrivateKey, ed25519.PublicKey, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, nil, fmt.Errorf("%w: private key length %d, want %d", ErrKeyCorrupted, len(priv), ed25519.PrivateKeySize)
	}
	derived := ed25519.NewKeyFromSeed(priv[:ed25519.SeedSize])
	if subtle.ConstantTimeCompare(priv, derived) != 1 {
		return nil, nil, fmt.Errorf("%w: private key seed and public tail disagree", ErrKeyCorrupted)
	}
	pub := append(ed25519.PublicKey(nil), derived[ed25519.SeedSize:]...)
	return derived, pub, nil
}

func validateKeyPair(kp KeyPair) error {
	_, derivedPub, err := validatePrivateKey(kp.Priv)
	if err != nil {
		return err
	}
	if len(kp.Pub) != ed25519.PublicKeySize || !keysEqual(kp.Pub, derivedPub) {
		return ErrPubKeyMismatch
	}
	return nil
}

func writeExclusiveKeyFile(path string, data []byte, perm os.FileMode) (err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		if os.IsExist(err) {
			return ErrKeyAlreadyExists
		}
		return err
	}
	keep := false
	defer func() {
		_ = f.Close()
		if !keep {
			_ = os.Remove(path)
		}
	}()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Chmod(perm); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	keep = true
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
	if err := validateKeyDirectories(projectRoot); err != nil {
		return KeyPair{}, err
	}

	info, err := os.Lstat(privPath)
	if err != nil {
		if os.IsNotExist(err) {
			return KeyPair{}, ErrKeyNotFound
		}
		return KeyPair{}, fmt.Errorf("stat private key: %w", err)
	}
	if info.Mode().Perm() != 0o400 {
		return KeyPair{}, fmt.Errorf("%w: %s has mode %o", ErrKeyPermsTooBroad, privPath, info.Mode().Perm())
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return KeyPair{}, fmt.Errorf("%w: private key is not a regular file", ErrKeyCorrupted)
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

	priv, pubFromPriv, err := validatePrivateKey(ed25519.PrivateKey(privBytes))
	if err != nil {
		return KeyPair{}, err
	}

	// The public anchor is mandatory and must match the key derived from the
	// private seed, so a missing or swapped half cannot sign.
	diskPub, err := loadPubFile(pubPath)
	if err != nil {
		return KeyPair{}, fmt.Errorf("load public key: %w", err)
	}
	if !keysEqual(diskPub, pubFromPriv) {
		return KeyPair{}, ErrPubKeyMismatch
	}

	return KeyPair{Priv: priv, Pub: pubFromPriv}, nil
}

// LoadPubKey reads only the public key (no permission check, no private
// file required). Callers that only need to verify signatures use this.
func LoadPubKey(projectRoot string) (ed25519.PublicKey, error) {
	if err := validateKeyDirectories(projectRoot); err != nil {
		return nil, err
	}
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
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrKeyNotFound
		}
		return nil, fmt.Errorf("lstat pub key %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: public key is not a regular file", ErrKeyCorrupted)
	}
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
