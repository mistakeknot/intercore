package authz

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLegacyManifest_DeterministicSignedRoundTrip(t *testing.T) {
	kp, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	marker, legacy := legacyManifestFixture()

	manifest, err := BuildLegacyManifest(kp.Pub, marker, []SignRow{legacy[1], legacy[0]})
	if err != nil {
		t.Fatalf("BuildLegacyManifest: %v", err)
	}
	if manifest.Schema != LegacyManifestSchema || manifest.Version != LegacyManifestVersion {
		t.Fatalf("manifest identity = %q/v%d", manifest.Schema, manifest.Version)
	}
	if manifest.LegacyCount != 2 || len(manifest.LegacyRows) != 2 {
		t.Fatalf("legacy membership = count %d rows %d", manifest.LegacyCount, len(manifest.LegacyRows))
	}
	if manifest.LegacyRows[0].ID != "legacy-a" || manifest.LegacyRows[1].ID != "legacy-b" {
		t.Fatalf("legacy rows not sorted: %+v", manifest.LegacyRows)
	}
	pubHash := sha256.Sum256(kp.Pub)
	if manifest.PublicKeySHA256 != hex.EncodeToString(pubHash[:]) {
		t.Fatalf("public key digest = %q", manifest.PublicKeySHA256)
	}
	if len(manifest.ManifestSHA256) != sha256.Size*2 {
		t.Fatalf("manifest digest length = %d", len(manifest.ManifestSHA256))
	}

	ordered, err := BuildLegacyManifest(kp.Pub, marker, legacy)
	if err != nil {
		t.Fatalf("BuildLegacyManifest ordered: %v", err)
	}
	if !reflect.DeepEqual(manifest, ordered) {
		t.Fatalf("manifest depends on input order\nunsorted: %+v\n ordered: %+v", manifest, ordered)
	}

	if err := SignLegacyManifest(kp.Priv, &manifest); err != nil {
		t.Fatalf("SignLegacyManifest: %v", err)
	}
	if manifest.SignatureAlgorithm != LegacyManifestSignatureAlgorithm || len(manifest.Signature) != 128 {
		t.Fatalf("signature metadata = %q/%d", manifest.SignatureAlgorithm, len(manifest.Signature))
	}
	if err := VerifyLegacyManifest(kp.Pub, manifest, marker, legacy); err != nil {
		t.Fatalf("VerifyLegacyManifest: %v", err)
	}
}

func TestLegacyManifest_RejectsMembershipAndAnchorTampering(t *testing.T) {
	kp, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	marker, legacy := legacyManifestFixture()
	manifest, err := BuildLegacyManifest(kp.Pub, marker, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := SignLegacyManifest(kp.Priv, &manifest); err != nil {
		t.Fatal(err)
	}
	other, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name     string
		pub      []byte
		manifest LegacyManifest
		marker   SignRow
		legacy   []SignRow
	}{
		{name: "wrong key", pub: other.Pub, manifest: manifest, marker: marker, legacy: legacy},
		{name: "missing row", pub: kp.Pub, manifest: manifest, marker: marker, legacy: legacy[:1]},
		{name: "inserted row", pub: kp.Pub, manifest: manifest, marker: marker, legacy: append(append([]SignRow{}, legacy...), SignRow{ID: "legacy-c", OpType: "bead-close", Target: "c", AgentID: "old", Mode: "auto", CreatedAt: 3})},
		{name: "mutated row", pub: kp.Pub, manifest: manifest, marker: marker, legacy: []SignRow{legacy[0], func() SignRow { r := legacy[1]; r.Target = "changed"; return r }()}},
		{name: "mutated marker", pub: kp.Pub, manifest: manifest, marker: func() SignRow { r := marker; r.CreatedAt++; return r }(), legacy: legacy},
		{name: "wrong marker identity", pub: kp.Pub, manifest: manifest, marker: func() SignRow { r := marker; r.ID = "forged-marker"; return r }(), legacy: legacy},
		{name: "unsupported version", pub: kp.Pub, manifest: func() LegacyManifest { m := manifest; m.Version++; return m }(), marker: marker, legacy: legacy},
		{name: "changed entry", pub: kp.Pub, manifest: func() LegacyManifest {
			m := manifest
			m.LegacyRows = append([]LegacyManifestRow(nil), m.LegacyRows...)
			m.LegacyRows[0].PayloadSHA256 = string(make([]byte, 64))
			return m
		}(), marker: marker, legacy: legacy},
		{name: "changed signature", pub: kp.Pub, manifest: func() LegacyManifest { m := manifest; m.Signature = flippedSignature(m.Signature); return m }(), marker: marker, legacy: legacy},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := VerifyLegacyManifest(tc.pub, tc.manifest, tc.marker, tc.legacy); err == nil {
				t.Fatal("tampered manifest state verified")
			}
		})
	}
}

func TestBuildLegacyManifest_RejectsAmbiguousInputs(t *testing.T) {
	kp, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	marker, legacy := legacyManifestFixture()

	t.Run("duplicate legacy id", func(t *testing.T) {
		rows := []SignRow{legacy[0], legacy[0]}
		if _, err := BuildLegacyManifest(kp.Pub, marker, rows); err == nil {
			t.Fatal("duplicate legacy IDs accepted")
		}
	})
	t.Run("wrong cutover id", func(t *testing.T) {
		marker.ID = "other"
		if _, err := BuildLegacyManifest(kp.Pub, marker, legacy); err == nil {
			t.Fatal("wrong cutover marker accepted")
		}
	})
}

func TestLegacyManifestFile_ExclusiveRegularFileRoundTrip(t *testing.T) {
	root := t.TempDir()
	kp, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	marker, legacy := legacyManifestFixture()
	manifest, err := BuildLegacyManifest(kp.Pub, marker, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := SignLegacyManifest(kp.Priv, &manifest); err != nil {
		t.Fatal(err)
	}
	if err := WriteKeyPair(root, kp); err != nil {
		t.Fatalf("WriteKeyPair: %v", err)
	}

	if err := WriteLegacyManifest(root, manifest); err != nil {
		t.Fatalf("WriteLegacyManifest: %v", err)
	}
	path := LegacyManifestPath(root)
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o444 {
		t.Fatalf("manifest mode = %v", info.Mode())
	}
	loaded, err := LoadLegacyManifest(root)
	if err != nil {
		t.Fatalf("LoadLegacyManifest: %v", err)
	}
	if !reflect.DeepEqual(loaded, manifest) {
		t.Fatalf("loaded manifest mismatch\n got: %+v\nwant: %+v", loaded, manifest)
	}
	if err := WriteLegacyManifest(root, manifest); !errors.Is(err, ErrLegacyManifestExists) {
		t.Fatalf("second write err=%v, want ErrLegacyManifestExists", err)
	}

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "outside.json")
	if err := os.WriteFile(target, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadLegacyManifest(root); err == nil {
		t.Fatal("LoadLegacyManifest followed a symlink")
	}
}

func TestWriteLegacyManifest_RejectsInvalidSignature(t *testing.T) {
	root := t.TempDir()
	kp, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteKeyPair(root, kp); err != nil {
		t.Fatal(err)
	}
	marker, legacy := legacyManifestFixture()
	manifest, err := BuildLegacyManifest(kp.Pub, marker, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := SignLegacyManifest(kp.Priv, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Signature = flippedSignature(manifest.Signature)
	if err := WriteLegacyManifest(root, manifest); err == nil {
		t.Fatal("WriteLegacyManifest accepted an invalid signature")
	}
	if _, err := os.Lstat(LegacyManifestPath(root)); !os.IsNotExist(err) {
		t.Fatalf("invalid manifest created a file: %v", err)
	}
}

func TestDecodeLegacyManifest_RejectsUnknownFieldsAndTrailingData(t *testing.T) {
	for _, input := range []string{
		`{"schema":"intercore.authz-legacy-manifest","version":1,"unknown":true}`,
		`{} {}`,
	} {
		if _, err := DecodeLegacyManifest([]byte(input)); err == nil {
			t.Fatalf("DecodeLegacyManifest accepted %q", input)
		}
	}
}

func legacyManifestFixture() (SignRow, []SignRow) {
	marker := SignRow{
		ID:        LegacyCutoverMarkerID,
		OpType:    "migration.signing-enabled",
		Target:    "authorizations",
		AgentID:   "system:migration-033",
		Mode:      "auto",
		CreatedAt: 100,
	}
	legacy := []SignRow{
		{ID: "legacy-a", OpType: "bead-close", Target: "a", AgentID: "old", Mode: "auto", CreatedAt: 1},
		{ID: "legacy-b", OpType: "git-push-main", Target: "b", AgentID: "old", Mode: "confirmed", CreatedAt: 2},
	}
	return marker, legacy
}

func flippedSignature(signature string) string {
	if signature[:2] == "00" {
		return "01" + signature[2:]
	}
	return "00" + signature[2:]
}
