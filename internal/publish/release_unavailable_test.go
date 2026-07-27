package publish

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const receiptLine = `printf '{"schema_version":1,"verified":true}\n'`

// TestVerifierUnavailableIsNotStale is the regression guard for mk-cg3z. It
// fails if "could not run" is ever folded back into "is stale" — which is how
// the original defect read as normal for as long as it did. Both halves of the
// assertion matter: the positive one alone would still pass if a future change
// wrapped BOTH sentinels around the same error.
func TestVerifierUnavailableIsNotStale(t *testing.T) {
	root := t.TempDir()
	writeReleaseScript(t, root, "verify-release-binaries.sh",
		"#!/usr/bin/env bash\necho 'verify-release-binaries: go is required' >&2\nexit 3\n")

	_, _, err := prepareReleaseArtifacts(root, true)
	if !errors.Is(err, ErrReleaseVerifierUnavailable) {
		t.Fatalf("err = %v, want ErrReleaseVerifierUnavailable", err)
	}
	if errors.Is(err, ErrStaleReleaseArtifacts) {
		t.Fatalf("err = %v, must NOT also be ErrStaleReleaseArtifacts", err)
	}
}

// TestStaleIsNotUnavailable guards the same boundary from the other side: a
// verifier that ran and found a genuine mismatch must not be excused as an
// environment problem, which would let real staleness publish.
func TestStaleIsNotUnavailable(t *testing.T) {
	root := t.TempDir()
	writeReleaseScript(t, root, "verify-release-binaries.sh",
		"#!/usr/bin/env bash\necho 'darwin-arm64 digest mismatch' >&2\nexit 1\n")

	_, _, err := prepareReleaseArtifacts(root, false)
	if !errors.Is(err, ErrStaleReleaseArtifacts) {
		t.Fatalf("err = %v, want ErrStaleReleaseArtifacts", err)
	}
	if errors.Is(err, ErrReleaseVerifierUnavailable) {
		t.Fatalf("err = %v, must NOT also be ErrReleaseVerifierUnavailable", err)
	}
}

// TestUnavailableNamesDependencyAndMachine covers the acceptance criterion
// directly: "go is required" is unactionable if you cannot tell which host
// said it.
func TestUnavailableNamesDependencyAndMachine(t *testing.T) {
	root := t.TempDir()
	writeReleaseScript(t, root, "verify-release-binaries.sh",
		"#!/usr/bin/env bash\necho 'verify-release-binaries: go is required' >&2\nexit 3\n")

	_, _, err := prepareReleaseArtifacts(root, true)
	if err == nil {
		t.Fatal("expected an error")
	}
	host, hostErr := os.Hostname()
	if hostErr != nil {
		t.Skip("hostname unavailable on this machine")
	}
	if !strings.Contains(err.Error(), host) {
		t.Errorf("err = %q, want it to name the machine %q", err, host)
	}
	if !strings.Contains(err.Error(), "go is required") {
		t.Errorf("err = %q, want it to name the missing dependency", err)
	}
}

// TestUnavailableVerifierDoesNotTriggerRebuild pins the compounding failure:
// the original code answered "cannot verify" by rebuilding, which failed for
// the identical missing dependency and reported staleness twice.
func TestUnavailableVerifierDoesNotTriggerRebuild(t *testing.T) {
	root := t.TempDir()
	trace := filepath.Join(root, "trace")
	t.Setenv("RELEASE_TRACE", trace)
	writeReleaseScript(t, root, "verify-release-binaries.sh",
		"#!/usr/bin/env bash\necho verify >>\"$RELEASE_TRACE\"\nexit 3\n")
	writeReleaseScript(t, root, "build-release.sh",
		"#!/usr/bin/env bash\necho build >>\"$RELEASE_TRACE\"\n")

	_, _, err := prepareReleaseArtifacts(root, true)
	if !errors.Is(err, ErrReleaseVerifierUnavailable) {
		t.Fatalf("err = %v, want ErrReleaseVerifierUnavailable", err)
	}
	data, readErr := os.ReadFile(trace)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(data) != "verify\n" {
		t.Fatalf("script trace = %q, want the verifier only (no rebuild attempt)", data)
	}
}

// TestBuilderUnavailableIsNotStale: the builder speaks the same protocol. A
// build that could not start says nothing about the artifacts.
func TestBuilderUnavailableIsNotStale(t *testing.T) {
	root := t.TempDir()
	writeReleaseScript(t, root, "verify-release-binaries.sh",
		"#!/usr/bin/env bash\necho 'CLI source changed after release build' >&2\nexit 1\n")
	writeReleaseScript(t, root, "build-release.sh",
		"#!/usr/bin/env bash\necho 'build-release: go is required' >&2\nexit 3\n")

	_, _, err := prepareReleaseArtifacts(root, true)
	if !errors.Is(err, ErrReleaseVerifierUnavailable) {
		t.Fatalf("err = %v, want ErrReleaseVerifierUnavailable", err)
	}
	if errors.Is(err, ErrStaleReleaseArtifacts) {
		t.Fatalf("err = %v, must NOT also be ErrStaleReleaseArtifacts", err)
	}
}

// TestExitZeroWithoutReceiptIsNotAPass covers the worse direction: a verifier
// that returns early checked nothing, and silence used to read as success.
func TestExitZeroWithoutReceiptIsNotAPass(t *testing.T) {
	root := t.TempDir()
	writeReleaseScript(t, root, "verify-release-binaries.sh",
		"#!/usr/bin/env bash\n# an added guard clause that short-circuits\nexit 0\n")

	_, _, err := prepareReleaseArtifacts(root, true)
	if !errors.Is(err, ErrReleaseVerifierUnavailable) {
		t.Fatalf("err = %v, want ErrReleaseVerifierUnavailable for a silent exit 0", err)
	}
	if !strings.Contains(err.Error(), "receipt") {
		t.Errorf("err = %q, want it to explain the missing receipt", err)
	}
}

// TestReceiptIsAcceptedAmongDiagnostics: verifiers write progress to stderr and
// output is captured combined, so the receipt has to be found in a stream, not
// parsed as the whole of it.
func TestReceiptIsAcceptedAmongDiagnostics(t *testing.T) {
	root := t.TempDir()
	writeReleaseScript(t, root, "verify-release-binaries.sh",
		"#!/usr/bin/env bash\necho 'checking darwin-arm64' >&2\n"+receiptLine+"\necho done >&2\nexit 0\n")

	files, rebuilt, err := prepareReleaseArtifacts(root, true)
	if err != nil {
		t.Fatalf("prepare with a valid receipt: %v", err)
	}
	if rebuilt || len(files) != 0 {
		t.Fatalf("clean verify returned files=%v rebuilt=%v", files, rebuilt)
	}
}

// TestVerifierThatCouldNotStartIsUnavailable covers the non-ExitError path:
// bash itself missing, the script not executable, the directory gone. Nothing
// was inspected, so nothing can be concluded.
func TestVerifierThatCouldNotStartIsUnavailable(t *testing.T) {
	root := t.TempDir()
	writeReleaseScript(t, root, "verify-release-binaries.sh", "#!/usr/bin/env bash\n"+receiptLine+"\n")

	original := execCommand
	t.Cleanup(func() { execCommand = original })
	execCommand = func(name string, arg ...string) *exec.Cmd {
		return exec.Command(filepath.Join(root, "definitely-not-an-executable"), arg...)
	}

	_, _, err := prepareReleaseArtifacts(root, true)
	if !errors.Is(err, ErrReleaseVerifierUnavailable) {
		t.Fatalf("err = %v, want ErrReleaseVerifierUnavailable", err)
	}
	if errors.Is(err, ErrStaleReleaseArtifacts) {
		t.Fatalf("err = %v, must NOT also be ErrStaleReleaseArtifacts", err)
	}
}

// TestClavainVerifierSpeaksTheProtocol checks the real script, not a fixture.
// A contract only one side implements is the failure this whole effort is
// about, so the shipped verifier is asserted to use the reserved exit code for
// its dependency checks and to emit a receipt on success.
func TestClavainVerifierSpeaksTheProtocol(t *testing.T) {
	script := filepath.Join("..", "..", "..", "..", "os", "Clavain", "scripts", releaseVerifyScript)
	body, err := os.ReadFile(script)
	if err != nil {
		t.Skipf("clavain checkout not present beside intercore: %v", err)
	}
	text := string(body)
	if !strings.Contains(text, fmt.Sprintf("exit %d", releaseUnavailableExit)) {
		t.Errorf("%s never uses the reserved unavailable exit code %d", script, releaseUnavailableExit)
	}
	if !strings.Contains(text, "verified:true") && !strings.Contains(text, `"verified":true`) {
		t.Errorf("%s does not emit a verification receipt", script)
	}
	for _, dep := range []string{"go is required", "jq is required", "git is required"} {
		if !strings.Contains(text, dep) {
			t.Errorf("%s no longer reports %q; the dependency contract moved", script, dep)
		}
	}
}
