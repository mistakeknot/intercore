package authz

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

const (
	LegacyManifestSchema             = "intercore.authz-legacy-manifest"
	LegacyManifestVersion            = 1
	LegacyManifestHashAlgorithm      = "sha256"
	LegacyManifestSignatureAlgorithm = "ed25519"
	LegacyCutoverMarkerID            = "migration-033-cutover-marker"
	legacyManifestFileName           = "authz-legacy-manifest.json"
	legacyManifestMaxBytes           = 1 << 20
)

const (
	legacyManifestDomain = "intercore-authz-legacy-manifest-v1"
	legacyMarkerDomain   = "intercore-authz-cutover-marker-v1"
	legacyRowDomain      = "intercore-authz-legacy-row-v1"
)

var (
	ErrLegacyManifestNotFound = errors.New("authz: legacy manifest not found")
	ErrLegacyManifestExists   = errors.New("authz: legacy manifest already exists")
	ErrLegacyManifestInvalid  = errors.New("authz: legacy manifest invalid")
)

// LegacyManifestRow identifies one canonical authorization payload by digest.
type LegacyManifestRow struct {
	ID            string `json:"id"`
	PayloadSHA256 string `json:"payload_sha256"`
}

// LegacyManifest is the public, signed anchor for the exact pre-signing set.
// ManifestSHA256 and Signature authenticate the deterministic body containing
// every preceding field; they are excluded from that body to avoid recursion.
type LegacyManifest struct {
	Schema             string              `json:"schema"`
	Version            int                 `json:"version"`
	HashAlgorithm      string              `json:"hash_algorithm"`
	SignatureAlgorithm string              `json:"signature_algorithm"`
	PublicKeySHA256    string              `json:"public_key_sha256"`
	CutoverMarker      LegacyManifestRow   `json:"cutover_marker"`
	LegacyCount        int                 `json:"legacy_count"`
	LegacyRows         []LegacyManifestRow `json:"legacy_rows"`
	ManifestSHA256     string              `json:"manifest_sha256"`
	Signature          string              `json:"signature"`
}

type legacyManifestBody struct {
	Schema             string              `json:"schema"`
	Version            int                 `json:"version"`
	HashAlgorithm      string              `json:"hash_algorithm"`
	SignatureAlgorithm string              `json:"signature_algorithm"`
	PublicKeySHA256    string              `json:"public_key_sha256"`
	CutoverMarker      LegacyManifestRow   `json:"cutover_marker"`
	LegacyCount        int                 `json:"legacy_count"`
	LegacyRows         []LegacyManifestRow `json:"legacy_rows"`
}

// BuildLegacyManifest constructs the deterministic unsigned anchor proposal.
func BuildLegacyManifest(pub ed25519.PublicKey, marker SignRow, legacyRows []SignRow) (LegacyManifest, error) {
	if len(pub) != ed25519.PublicKeySize {
		return LegacyManifest{}, fmt.Errorf("%w: public key length %d", ErrLegacyManifestInvalid, len(pub))
	}
	if err := validateLegacyMarker(marker); err != nil {
		return LegacyManifest{}, err
	}

	rows := make([]LegacyManifestRow, 0, len(legacyRows))
	seen := make(map[string]struct{}, len(legacyRows))
	for _, row := range legacyRows {
		if row.ID == "" {
			return LegacyManifest{}, fmt.Errorf("%w: empty legacy row id", ErrLegacyManifestInvalid)
		}
		if _, ok := seen[row.ID]; ok {
			return LegacyManifest{}, fmt.Errorf("%w: duplicate legacy row id %q", ErrLegacyManifestInvalid, row.ID)
		}
		seen[row.ID] = struct{}{}
		digest, err := domainPayloadDigest(legacyRowDomain, row)
		if err != nil {
			return LegacyManifest{}, fmt.Errorf("%w: legacy row %s: %v", ErrLegacyManifestInvalid, row.ID, err)
		}
		rows = append(rows, LegacyManifestRow{ID: row.ID, PayloadSHA256: digest})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })

	markerDigest, err := domainPayloadDigest(legacyMarkerDomain, marker)
	if err != nil {
		return LegacyManifest{}, fmt.Errorf("%w: cutover marker: %v", ErrLegacyManifestInvalid, err)
	}
	pubDigest := sha256.Sum256(pub)
	manifest := LegacyManifest{
		Schema:             LegacyManifestSchema,
		Version:            LegacyManifestVersion,
		HashAlgorithm:      LegacyManifestHashAlgorithm,
		SignatureAlgorithm: LegacyManifestSignatureAlgorithm,
		PublicKeySHA256:    hex.EncodeToString(pubDigest[:]),
		CutoverMarker: LegacyManifestRow{
			ID:            marker.ID,
			PayloadSHA256: markerDigest,
		},
		LegacyCount: len(rows),
		LegacyRows:  rows,
	}
	payload, err := canonicalLegacyManifestPayload(manifest)
	if err != nil {
		return LegacyManifest{}, err
	}
	digest := sha256.Sum256(payload)
	manifest.ManifestSHA256 = hex.EncodeToString(digest[:])
	return manifest, nil
}

// SignLegacyManifest signs a previously built proposal without changing its
// digest or membership.
func SignLegacyManifest(priv ed25519.PrivateKey, manifest *LegacyManifest) error {
	if manifest == nil {
		return fmt.Errorf("%w: nil manifest", ErrLegacyManifestInvalid)
	}
	validated, pub, err := validatePrivateKey(priv)
	if err != nil {
		return err
	}
	payload, err := canonicalLegacyManifestPayload(*manifest)
	if err != nil {
		return err
	}
	if err := validateManifestDigests(pub, *manifest, payload); err != nil {
		return err
	}
	manifest.Signature = hex.EncodeToString(ed25519.Sign(validated, payload))
	return nil
}

// VerifyLegacyManifest authenticates the manifest and proves it describes the
// exact marker and legacy row set supplied by the caller.
func VerifyLegacyManifest(pub ed25519.PublicKey, manifest LegacyManifest, marker SignRow, legacyRows []SignRow) error {
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: public key length %d", ErrLegacyManifestInvalid, len(pub))
	}
	payload, err := canonicalLegacyManifestPayload(manifest)
	if err != nil {
		return err
	}
	if err := validateManifestDigests(pub, manifest, payload); err != nil {
		return err
	}
	sig, err := decodeFixedHex("signature", manifest.Signature, ed25519.SignatureSize)
	if err != nil {
		return err
	}
	if !ed25519.Verify(pub, payload, sig) {
		return fmt.Errorf("%w: signature verification failed", ErrLegacyManifestInvalid)
	}
	expected, err := BuildLegacyManifest(pub, marker, legacyRows)
	if err != nil {
		return err
	}
	if !equalDigest(manifest.ManifestSHA256, expected.ManifestSHA256) {
		return fmt.Errorf("%w: anchored legacy set does not match ledger", ErrLegacyManifestInvalid)
	}
	return nil
}

// LegacyManifestPath returns the conventional public anchor path.
func LegacyManifestPath(projectRoot string) string {
	return filepath.Join(projectRoot, ".clavain", "keys", legacyManifestFileName)
}

// WriteLegacyManifest writes a signed manifest exactly once as a public,
// read-only regular file. There is deliberately no overwrite path.
func WriteLegacyManifest(projectRoot string, manifest LegacyManifest) error {
	if manifest.Signature == "" {
		return fmt.Errorf("%w: unsigned manifest", ErrLegacyManifestInvalid)
	}
	encoded, err := encodeLegacyManifest(manifest)
	if err != nil {
		return err
	}
	path := LegacyManifestPath(projectRoot)
	dir := filepath.Dir(path)
	if err := ensureRealDirectory(filepath.Dir(dir), false); err != nil {
		return fmt.Errorf("prepare .clavain dir: %w", err)
	}
	if err := ensureRealDirectory(dir, true); err != nil {
		return fmt.Errorf("prepare keys dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o444)
	if err != nil {
		if os.IsExist(err) {
			return ErrLegacyManifestExists
		}
		return fmt.Errorf("write legacy manifest: %w", err)
	}
	keep := false
	defer func() {
		_ = f.Close()
		if !keep {
			_ = os.Remove(path)
		}
	}()
	if _, err := f.Write(encoded); err != nil {
		return fmt.Errorf("write legacy manifest: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync legacy manifest: %w", err)
	}
	if err := f.Chmod(0o444); err != nil {
		return fmt.Errorf("chmod legacy manifest: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close legacy manifest: %w", err)
	}
	keep = true
	return nil
}

// LoadLegacyManifest reads a bounded regular file without following symlinks.
func LoadLegacyManifest(projectRoot string) (LegacyManifest, error) {
	path := LegacyManifestPath(projectRoot)
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return LegacyManifest{}, ErrLegacyManifestNotFound
		}
		return LegacyManifest{}, fmt.Errorf("lstat legacy manifest: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return LegacyManifest{}, fmt.Errorf("%w: manifest is not a regular file", ErrLegacyManifestInvalid)
	}
	if info.Size() > legacyManifestMaxBytes {
		return LegacyManifest{}, fmt.Errorf("%w: manifest exceeds %d bytes", ErrLegacyManifestInvalid, legacyManifestMaxBytes)
	}
	f, err := os.Open(path)
	if err != nil {
		return LegacyManifest{}, fmt.Errorf("open legacy manifest: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, legacyManifestMaxBytes+1))
	if err != nil {
		return LegacyManifest{}, fmt.Errorf("read legacy manifest: %w", err)
	}
	if len(data) > legacyManifestMaxBytes {
		return LegacyManifest{}, fmt.Errorf("%w: manifest exceeds %d bytes", ErrLegacyManifestInvalid, legacyManifestMaxBytes)
	}
	return DecodeLegacyManifest(data)
}

// DecodeLegacyManifest parses one strict JSON object and rejects extensions or
// trailing data so all verifiers agree on the artifact contract.
func DecodeLegacyManifest(data []byte) (LegacyManifest, error) {
	var manifest LegacyManifest
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&manifest); err != nil {
		return LegacyManifest{}, fmt.Errorf("%w: decode: %v", ErrLegacyManifestInvalid, err)
	}
	var extra interface{}
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return LegacyManifest{}, fmt.Errorf("%w: trailing JSON value", ErrLegacyManifestInvalid)
		}
		return LegacyManifest{}, fmt.Errorf("%w: trailing data: %v", ErrLegacyManifestInvalid, err)
	}
	payload, err := canonicalLegacyManifestPayload(manifest)
	if err != nil {
		return LegacyManifest{}, err
	}
	digest := sha256.Sum256(payload)
	if !equalDigest(manifest.ManifestSHA256, hex.EncodeToString(digest[:])) {
		return LegacyManifest{}, fmt.Errorf("%w: manifest digest mismatch", ErrLegacyManifestInvalid)
	}
	if _, err := decodeFixedHex("signature", manifest.Signature, ed25519.SignatureSize); err != nil {
		return LegacyManifest{}, err
	}
	return manifest, nil
}

func encodeLegacyManifest(manifest LegacyManifest) ([]byte, error) {
	if _, err := DecodeLegacyManifest(mustMarshalManifest(manifest)); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("%w: encode: %v", ErrLegacyManifestInvalid, err)
	}
	return append(data, '\n'), nil
}

func mustMarshalManifest(manifest LegacyManifest) []byte {
	data, _ := json.Marshal(manifest)
	return data
}

func canonicalLegacyManifestPayload(manifest LegacyManifest) ([]byte, error) {
	if manifest.Schema != LegacyManifestSchema || manifest.Version != LegacyManifestVersion {
		return nil, fmt.Errorf("%w: unsupported schema %q version %d", ErrLegacyManifestInvalid, manifest.Schema, manifest.Version)
	}
	if manifest.HashAlgorithm != LegacyManifestHashAlgorithm || manifest.SignatureAlgorithm != LegacyManifestSignatureAlgorithm {
		return nil, fmt.Errorf("%w: unsupported algorithms", ErrLegacyManifestInvalid)
	}
	if _, err := decodeFixedHex("public_key_sha256", manifest.PublicKeySHA256, sha256.Size); err != nil {
		return nil, err
	}
	if manifest.CutoverMarker.ID != LegacyCutoverMarkerID {
		return nil, fmt.Errorf("%w: cutover marker id %q", ErrLegacyManifestInvalid, manifest.CutoverMarker.ID)
	}
	if _, err := decodeFixedHex("cutover marker digest", manifest.CutoverMarker.PayloadSHA256, sha256.Size); err != nil {
		return nil, err
	}
	if manifest.LegacyRows == nil || manifest.LegacyCount != len(manifest.LegacyRows) {
		return nil, fmt.Errorf("%w: legacy count %d does not match rows %d", ErrLegacyManifestInvalid, manifest.LegacyCount, len(manifest.LegacyRows))
	}
	previous := ""
	for i, row := range manifest.LegacyRows {
		if row.ID == "" || (i > 0 && row.ID <= previous) {
			return nil, fmt.Errorf("%w: legacy rows must have unique ascending IDs", ErrLegacyManifestInvalid)
		}
		if _, err := decodeFixedHex("legacy row digest", row.PayloadSHA256, sha256.Size); err != nil {
			return nil, err
		}
		previous = row.ID
	}
	body := legacyManifestBody{
		Schema:             manifest.Schema,
		Version:            manifest.Version,
		HashAlgorithm:      manifest.HashAlgorithm,
		SignatureAlgorithm: manifest.SignatureAlgorithm,
		PublicKeySHA256:    manifest.PublicKeySHA256,
		CutoverMarker:      manifest.CutoverMarker,
		LegacyCount:        manifest.LegacyCount,
		LegacyRows:         manifest.LegacyRows,
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("%w: canonical body: %v", ErrLegacyManifestInvalid, err)
	}
	payload := make([]byte, 0, len(legacyManifestDomain)+1+len(encoded))
	payload = append(payload, legacyManifestDomain...)
	payload = append(payload, 0)
	payload = append(payload, encoded...)
	return payload, nil
}

func validateManifestDigests(pub ed25519.PublicKey, manifest LegacyManifest, payload []byte) error {
	pubDigest := sha256.Sum256(pub)
	if !equalDigest(manifest.PublicKeySHA256, hex.EncodeToString(pubDigest[:])) {
		return fmt.Errorf("%w: public key digest mismatch", ErrLegacyManifestInvalid)
	}
	digest := sha256.Sum256(payload)
	if !equalDigest(manifest.ManifestSHA256, hex.EncodeToString(digest[:])) {
		return fmt.Errorf("%w: manifest digest mismatch", ErrLegacyManifestInvalid)
	}
	return nil
}

func validateLegacyMarker(marker SignRow) error {
	if marker.ID != LegacyCutoverMarkerID ||
		marker.OpType != "migration.signing-enabled" ||
		marker.Target != "authorizations" ||
		marker.AgentID != "system:migration-033" ||
		marker.Mode != "auto" {
		return fmt.Errorf("%w: unexpected cutover marker identity", ErrLegacyManifestInvalid)
	}
	return nil
}

func domainPayloadDigest(domain string, row SignRow) (string, error) {
	payload, err := CanonicalPayload(row)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, _ = h.Write([]byte(domain))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(payload)
	return hex.EncodeToString(h.Sum(nil)), nil
}

func decodeFixedHex(name, value string, size int) ([]byte, error) {
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != size {
		return nil, fmt.Errorf("%w: %s must be %d-byte lowercase hex", ErrLegacyManifestInvalid, name, size)
	}
	if value != hex.EncodeToString(decoded) {
		return nil, fmt.Errorf("%w: %s is not canonical lowercase hex", ErrLegacyManifestInvalid, name)
	}
	return decoded, nil
}

func equalDigest(a, b string) bool {
	aBytes, aErr := hex.DecodeString(a)
	bBytes, bErr := hex.DecodeString(b)
	return aErr == nil && bErr == nil && len(aBytes) == len(bBytes) && subtle.ConstantTimeCompare(aBytes, bBytes) == 1
}
