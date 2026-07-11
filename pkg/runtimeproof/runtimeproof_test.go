package runtimeproof

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var fixedNow = time.Date(2026, 7, 10, 22, 0, 0, 0, time.UTC)

func digestBytes(b []byte) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256(b))
}

func validReceipt(root, buildPath, installedPath string) Receipt {
	started := fixedNow.Add(-2 * time.Minute)
	created := fixedNow.Add(-time.Minute)
	return Receipt{
		SchemaVersion: 1,
		Subject: Subject{
			BeadID:      "sylveste-6h7x",
			RunID:       "run-123",
			ProjectRoot: root,
			GitHead:     strings.Repeat("a", 40),
			Host:        "clavain",
			CreatedAt:   created.Format(time.RFC3339Nano),
		},
		Artifact: Artifact{
			Kind:            "file",
			BuildPath:       buildPath,
			InstalledPath:   installedPath,
			BuildDigest:     digestBytes([]byte("runtime-binary")),
			InstalledDigest: digestBytes([]byte("runtime-binary")),
			RuntimeDigest:   digestBytes([]byte("runtime-binary")),
		},
		Boot: Boot{
			StartedForProbe: true,
			ProcessID:       4242,
			StartedAt:       started.Format(time.RFC3339Nano),
			InstanceNonce:   "nonce-123",
			ObservedNonce:   "nonce-123",
			State:           StateVerified,
		},
		Health: Health{
			RequiredSubsystems: []string{"store"},
			Observed:           map[string]string{"store": "healthy"},
			FailureClasses: map[string]State{
				"startup":              StateVerified,
				"dependency_injection": StateNotApplicable,
				"connection":           StateVerified,
				"projection_catchup":   StateNotApplicable,
			},
		},
		Event: Event{
			EventID:         "event-123",
			ObservedEventID: "event-123",
			BeforeDigest:    digestBytes([]byte("before")),
			AfterDigest:     digestBytes([]byte("after")),
			Assertions: []Assertion{{
				Name:     "state-delta",
				State:    StateVerified,
				Evidence: "counter advanced from 0 to 1",
			}},
		},
		SurfaceScan: SurfaceScan{
			Expected:   []string{"health", "event"},
			Observed:   []string{"event", "health"},
			Missing:    []string{},
			Unexpected: []string{},
		},
		Isolation: Isolation{
			Resources: []Resource{{
				Kind:        "port",
				Fingerprint: digestBytes([]byte("127.0.0.1:49152")),
				Ownership:   "ephemeral",
			}},
			Collisions: []string{},
		},
		Cleanup: Cleanup{OwnedResourcesRemaining: []string{}},
	}
}

func validExpectations(buildPath, installedPath string) Expectations {
	return Expectations{
		ExpectedBuildPath:     buildPath,
		ExpectedInstalledPath: installedPath,
		RequiredSubsystems:    []string{"store"},
		NotApplicableFailureClasses: map[string]bool{
			"dependency_injection": true,
			"projection_catchup":   true,
		},
		RequiredAssertions: []string{"state-delta"},
		ExpectedSurfaces:   []string{"health", "event"},
		RequiredResources: []ResourceExpectation{{
			Kind:      "port",
			Ownership: "ephemeral",
		}},
	}
}

func writeFixture(t *testing.T, mutate func(*Receipt)) (string, VerifyOptions) {
	t.Helper()
	root := t.TempDir()
	buildPath := filepath.Join(root, "build", "server")
	installedPath := filepath.Join(root, "bin", "server")
	for _, p := range []string{buildPath, installedPath} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("runtime-binary"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	receipt := validReceipt(root, buildPath, installedPath)
	if mutate != nil {
		mutate(&receipt)
	}
	b, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	receiptPath := filepath.Join(root, "receipt.json")
	if err := os.WriteFile(receiptPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return receiptPath, VerifyOptions{
		ExpectedBeadID:       "sylveste-6h7x",
		ExpectedRunID:        "run-123",
		ExpectedProjectRoot:  root,
		ExpectedArtifactHash: digestBytes(b),
		RunCreatedAt:         fixedNow.Add(-time.Hour),
		Expectations:         validExpectations(buildPath, installedPath),
		Environment: Environment{
			Now:      func() time.Time { return fixedNow },
			Hostname: func() (string, error) { return "clavain", nil },
			GitHead: func(context.Context, string) (string, error) {
				return strings.Repeat("a", 40), nil
			},
		},
	}
}

func TestVerifyFileRejectsArtifactPathSubstitution(t *testing.T) {
	path, opts := writeFixture(t, nil)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var receipt Receipt
	if err := json.Unmarshal(b, &receipt); err != nil {
		t.Fatal(err)
	}
	fakeBuild := filepath.Join(filepath.Dir(receipt.Artifact.BuildPath), "other-server")
	if err := os.WriteFile(fakeBuild, []byte("runtime-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	receipt.Artifact.BuildPath = fakeBuild
	b, err = json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	opts.ExpectedArtifactHash = digestBytes(b)
	if _, err := VerifyFile(context.Background(), path, opts); err == nil || !strings.Contains(err.Error(), "build_path") {
		t.Fatalf("error = %v, want build_path mismatch", err)
	}
}

func TestVerifyFileRejectsSelfAuthoredScope(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Receipt)
		want   string
	}{
		{
			"reduced subsystems",
			func(r *Receipt) {
				r.Health.RequiredSubsystems = []string{"cache"}
				r.Health.Observed = map[string]string{"cache": "healthy"}
			},
			"required subsystems",
		},
		{
			"unauthorized not applicable",
			func(r *Receipt) { r.Health.FailureClasses["connection"] = StateNotApplicable },
			"connection",
		},
		{
			"substituted assertion",
			func(r *Receipt) { r.Event.Assertions[0].Name = "different-check" },
			"required assertions",
		},
		{
			"reduced surfaces",
			func(r *Receipt) {
				r.SurfaceScan.Expected = []string{"health"}
				r.SurfaceScan.Observed = []string{"health"}
			},
			"expected surfaces",
		},
		{
			"substituted resource",
			func(r *Receipt) { r.Isolation.Resources[0].Kind = "path" },
			"required resources",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, opts := writeFixture(t, tt.mutate)
			_, err := VerifyFile(context.Background(), path, opts)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), tt.want) {
				t.Fatalf("error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestVerifyFileRequiresTrustedExpectations(t *testing.T) {
	path, opts := writeFixture(t, nil)
	opts.Expectations = Expectations{}
	if _, err := VerifyFile(context.Background(), path, opts); err == nil || !strings.Contains(err.Error(), "expectations") {
		t.Fatalf("error = %v, want missing expectations", err)
	}
}

func TestVerifyFileValid(t *testing.T) {
	path, opts := writeFixture(t, nil)
	result, err := VerifyFile(context.Background(), path, opts)
	if err != nil {
		t.Fatalf("VerifyFile: %v", err)
	}
	if result.ProofHash != opts.ExpectedArtifactHash {
		t.Fatalf("proof hash = %q, want %q", result.ProofHash, opts.ExpectedArtifactHash)
	}
	if result.Summary.ProofHash != result.ProofHash || result.Summary.RunID != "run-123" {
		t.Fatalf("bad summary: %+v", result.Summary)
	}
	if result.Summary.HostFingerprint == "" || strings.Contains(result.Summary.HostFingerprint, "clavain") {
		t.Fatalf("host fingerprint was not sanitized: %q", result.Summary.HostFingerprint)
	}
}

func TestVerifyFileFailClosed(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Receipt)
		want   string
	}{
		{"wrong bead", func(r *Receipt) { r.Subject.BeadID = "other" }, "bead_id"},
		{"wrong run", func(r *Receipt) { r.Subject.RunID = "other" }, "run_id"},
		{"wrong root", func(r *Receipt) { r.Subject.ProjectRoot = "/tmp/other" }, "project_root"},
		{"wrong head", func(r *Receipt) { r.Subject.GitHead = strings.Repeat("b", 40) }, "git_head"},
		{"wrong host", func(r *Receipt) { r.Subject.Host = "zklw" }, "host"},
		{"stale", func(r *Receipt) { r.Subject.CreatedAt = fixedNow.Add(-25 * time.Hour).Format(time.RFC3339) }, "stale"},
		{"predates run", func(r *Receipt) { r.Subject.CreatedAt = fixedNow.Add(-2 * time.Hour).Format(time.RFC3339) }, "predates"},
		{"build digest", func(r *Receipt) { r.Artifact.BuildDigest = digestBytes([]byte("wrong")) }, "build_digest"},
		{"installed digest", func(r *Receipt) { r.Artifact.InstalledDigest = digestBytes([]byte("wrong")) }, "installed_digest"},
		{"runtime digest", func(r *Receipt) { r.Artifact.RuntimeDigest = digestBytes([]byte("wrong")) }, "runtime_digest"},
		{"not started", func(r *Receipt) { r.Boot.StartedForProbe = false }, "started_for_probe"},
		{"bad pid", func(r *Receipt) { r.Boot.ProcessID = 0 }, "process_id"},
		{"boot chronology", func(r *Receipt) { r.Boot.StartedAt = fixedNow.Add(time.Minute).Format(time.RFC3339) }, "started_at"},
		{"nonce mismatch", func(r *Receipt) { r.Boot.ObservedNonce = "other" }, "nonce"},
		{"boot unverifiable", func(r *Receipt) { r.Boot.State = StateUnverifiable }, "boot.state"},
		{"missing subsystem", func(r *Receipt) { delete(r.Health.Observed, "store") }, "subsystem"},
		{"unhealthy subsystem", func(r *Receipt) { r.Health.Observed["store"] = "degraded" }, "healthy"},
		{"startup not applicable", func(r *Receipt) { r.Health.FailureClasses["startup"] = StateNotApplicable }, "startup"},
		{"failure unverifiable", func(r *Receipt) { r.Health.FailureClasses["connection"] = StateUnverifiable }, "connection"},
		{"event mismatch", func(r *Receipt) { r.Event.ObservedEventID = "other" }, "event"},
		{"no state delta", func(r *Receipt) { r.Event.AfterDigest = r.Event.BeforeDigest }, "state delta"},
		{"no assertions", func(r *Receipt) { r.Event.Assertions = nil }, "assertion"},
		{"failed assertion", func(r *Receipt) { r.Event.Assertions[0].State = StateFailed }, "assertion"},
		{"surface missing", func(r *Receipt) { r.SurfaceScan.Missing = []string{"event"} }, "surface"},
		{"surface mismatch", func(r *Receipt) { r.SurfaceScan.Observed = []string{"health"} }, "surface"},
		{"shared resource", func(r *Receipt) { r.Isolation.Resources[0].Ownership = "shared" }, "ownership"},
		{"database resource", func(r *Receipt) { r.Isolation.Resources[0].Kind = "database" }, "resource kind"},
		{"collision", func(r *Receipt) { r.Isolation.Collisions = []string{"port"} }, "collision"},
		{"cleanup", func(r *Receipt) { r.Cleanup.OwnedResourcesRemaining = []string{"port"} }, "cleanup"},
		{"missing surface findings", func(r *Receipt) { r.SurfaceScan.Missing = nil }, "missing field"},
		{"missing collisions", func(r *Receipt) { r.Isolation.Collisions = nil }, "missing field"},
		{"missing cleanup", func(r *Receipt) { r.Cleanup.OwnedResourcesRemaining = nil }, "missing field"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, opts := writeFixture(t, tt.mutate)
			_, err := VerifyFile(context.Background(), path, opts)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tt.want)) {
				t.Fatalf("error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestReadRegularFileRejectsSwapToSymlink(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "candidate")
	target := filepath.Join(root, "target")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := readRegularFileWithHook(path, 1024, func() {
		if removeErr := os.Remove(path); removeErr != nil {
			t.Fatal(removeErr)
		}
		if linkErr := os.Symlink(target, path); linkErr != nil {
			t.Fatal(linkErr)
		}
	})
	if err == nil {
		t.Fatal("swap to symlink was accepted")
	}
}

func TestVerifyFileRejectsStrictJSONAndArtifactHashMismatch(t *testing.T) {
	path, opts := writeFixture(t, nil)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b = append(b[:len(b)-1], []byte(`,"unknown":true}`)...)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	opts.ExpectedArtifactHash = digestBytes(b)
	if _, err := VerifyFile(context.Background(), path, opts); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("unknown-field error = %v", err)
	}

	path, opts = writeFixture(t, nil)
	opts.ExpectedArtifactHash = digestBytes([]byte("other"))
	if _, err := VerifyFile(context.Background(), path, opts); err == nil || !strings.Contains(err.Error(), "content hash") {
		t.Fatalf("hash mismatch error = %v", err)
	}
}

func TestVerifyFileRejectsSymlinksAndOversizedFiles(t *testing.T) {
	path, opts := writeFixture(t, nil)
	symlink := filepath.Join(filepath.Dir(path), "receipt-link.json")
	if err := os.Symlink(path, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyFile(context.Background(), symlink, opts); err == nil || !strings.Contains(err.Error(), "regular") {
		t.Fatalf("symlink error = %v", err)
	}

	path, opts = writeFixture(t, nil)
	opts.MaxReceiptBytes = 32
	if _, err := VerifyFile(context.Background(), path, opts); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("receipt size error = %v", err)
	}

	path, opts = writeFixture(t, nil)
	opts.MaxArtifactBytes = 4
	if _, err := VerifyFile(context.Background(), path, opts); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("artifact size error = %v", err)
	}
}

func TestVerifyFileHashesAndDecodesSameReceiptRead(t *testing.T) {
	path, opts := writeFixture(t, nil)
	reads := 0
	opts.Environment.ReadRegularFile = func(p string, limit int64) ([]byte, error) {
		b, err := readRegularFile(p, limit)
		if p == path {
			reads++
		}
		return b, err
	}
	if _, err := VerifyFile(context.Background(), path, opts); err != nil {
		t.Fatal(err)
	}
	if reads != 1 {
		t.Fatalf("receipt reads = %d, want 1", reads)
	}
}

func TestVerifyFilePropagatesBoundedGitFailure(t *testing.T) {
	path, opts := writeFixture(t, nil)
	opts.Environment.GitHead = func(context.Context, string) (string, error) {
		return "", context.DeadlineExceeded
	}
	if _, err := VerifyFile(context.Background(), path, opts); err == nil || !strings.Contains(err.Error(), "git head") {
		t.Fatalf("git error = %v", err)
	}
}
