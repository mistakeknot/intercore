package publish

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeReleaseScript(t *testing.T, root, name, body string) {
	t.Helper()
	dir := filepath.Join(root, "scripts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareReleaseArtifactsNoVerifierIsNoOp(t *testing.T) {
	files, rebuilt, err := prepareReleaseArtifacts(t.TempDir(), true)
	if err != nil {
		t.Fatalf("prepare unmanaged plugin: %v", err)
	}
	if rebuilt || len(files) != 0 {
		t.Fatalf("unmanaged plugin returned files=%v rebuilt=%v", files, rebuilt)
	}
}

func TestPrepareReleaseArtifactsRejectsStaleReleaseWithoutBuilder(t *testing.T) {
	root := t.TempDir()
	writeReleaseScript(t, root, "verify-release-binaries.sh", "#!/usr/bin/env bash\nexit 1\n")

	_, _, err := prepareReleaseArtifacts(root, true)
	if !errors.Is(err, ErrStaleReleaseArtifacts) || !strings.Contains(err.Error(), "build-release.sh is missing") {
		t.Fatalf("prepare error = %v, want stale release with missing builder", err)
	}
}

func TestPrepareReleaseArtifactsReportsBuilderFailure(t *testing.T) {
	root := t.TempDir()
	writeReleaseScript(t, root, "verify-release-binaries.sh", "#!/usr/bin/env bash\necho stale >&2\nexit 1\n")
	writeReleaseScript(t, root, "build-release.sh", "#!/usr/bin/env bash\necho compiler-failed >&2\nexit 7\n")

	_, _, err := prepareReleaseArtifacts(root, true)
	if !errors.Is(err, ErrStaleReleaseArtifacts) || !strings.Contains(err.Error(), "compiler-failed") {
		t.Fatalf("prepare error = %v, want stale release with builder output", err)
	}
}

func TestPrepareReleaseArtifactsRejectsFailedPostBuildVerification(t *testing.T) {
	root := t.TempDir()
	writeReleaseScript(t, root, "verify-release-binaries.sh", "#!/usr/bin/env bash\necho still-stale >&2\nexit 1\n")
	writeReleaseScript(t, root, "build-release.sh", "#!/usr/bin/env bash\nexit 0\n")

	_, _, err := prepareReleaseArtifacts(root, true)
	if !errors.Is(err, ErrStaleReleaseArtifacts) || !strings.Contains(err.Error(), "still-stale") {
		t.Fatalf("prepare error = %v, want failed post-build verification", err)
	}
}

func TestPrepareReleaseArtifactsDoesNotBuildWhenDisallowed(t *testing.T) {
	root := t.TempDir()
	trace := filepath.Join(root, "trace")
	t.Setenv("RELEASE_TRACE", trace)
	writeReleaseScript(t, root, "verify-release-binaries.sh", "#!/usr/bin/env bash\necho verify >>\"$RELEASE_TRACE\"\nexit 1\n")
	writeReleaseScript(t, root, "build-release.sh", "#!/usr/bin/env bash\necho build >>\"$RELEASE_TRACE\"\n")

	_, _, err := prepareReleaseArtifacts(root, false)
	if !errors.Is(err, ErrStaleReleaseArtifacts) {
		t.Fatalf("prepare error = %v, want stale release rejection", err)
	}
	data, readErr := os.ReadFile(trace)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(data) != "verify\n" {
		t.Fatalf("release script order = %q, want verifier only", data)
	}
}
