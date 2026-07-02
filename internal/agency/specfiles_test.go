package agency

import (
	"os"
	"path/filepath"
	"testing"
)

// testdataSpecDir holds byte-for-byte copies of the production agency specs
// (one per macro-stage) so this test provides real coverage in any checkout,
// including standalone clones and CI where the sibling Clavain repo is absent.
// Keep these in sync with the canonical specs under os/Clavain/config/agency/.
const testdataSpecDir = "testdata/specs"

// liveSpecCandidates are the locations where the canonical production specs may
// live relative to this package, in priority order. The first existing one is
// validated in addition to the bundled fixtures so drift in the real specs is
// caught when the test runs inside the monorepo. Override with the
// INTERCORE_AGENCY_SPEC_DIR environment variable.
var liveSpecCandidates = []string{
	"../../../../os/Clavain/config/agency",  // monorepo: core/intercore alongside os/Clavain
	"../../../../hub/clavain/config/agency", // legacy location (pre-os/ rename)
}

// validateSpecDir parses every *.yaml in dir, validates it against testPhases,
// and asserts the per-stage invariants. wantCount, when > 0, asserts the exact
// number of spec files found (one per macro-stage).
func validateSpecDir(t *testing.T, dir string, wantCount int) {
	t.Helper()

	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		t.Fatalf("glob %s: %v", dir, err)
	}
	if len(files) == 0 {
		t.Fatalf("no *.yaml spec files found in %s", dir)
	}
	if wantCount > 0 && len(files) != wantCount {
		t.Fatalf("expected %d spec files in %s, got %d", wantCount, dir, len(files))
	}

	for _, f := range files {
		name := filepath.Base(f)
		t.Run(name, func(t *testing.T) {
			spec, err := ParseFile(f)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			errs := Validate(spec, testPhases)
			for _, e := range errs {
				t.Errorf("%s", e)
			}
			if spec.Meta.Stage == "" {
				t.Error("meta.stage is empty")
			}
			if len(spec.Meta.Phases) == 0 {
				t.Error("meta.phases is empty")
			}
		})
	}
}

// TestBundledSpecFiles validates the spec fixtures bundled under testdata/specs.
// These are copies of the production specs, so this test always runs and gives
// real parse + validate coverage regardless of where the test is executed.
func TestBundledSpecFiles(t *testing.T) {
	if _, err := os.Stat(testdataSpecDir); err != nil {
		t.Fatalf("bundled spec fixtures missing at %s: %v", testdataSpecDir, err)
	}
	// One spec per macro-stage: discover, design, build, ship, reflect.
	validateSpecDir(t, testdataSpecDir, len(KnownStages))
}

// TestLiveSpecFiles validates the canonical production specs when they are
// reachable from this checkout (i.e. running inside the Sylveste monorepo). It
// catches drift in the real specs that the bundled fixtures would not. When the
// specs are absent (standalone clone, CI), it skips with an actionable message
// naming exactly what was checked.
func TestLiveSpecFiles(t *testing.T) {
	var checked []string

	if dir := os.Getenv("INTERCORE_AGENCY_SPEC_DIR"); dir != "" {
		if _, err := os.Stat(dir); err == nil {
			validateSpecDir(t, dir, 0)
			return
		}
		checked = append(checked, dir+" (from INTERCORE_AGENCY_SPEC_DIR)")
	}

	for _, dir := range liveSpecCandidates {
		if _, err := os.Stat(dir); err == nil {
			validateSpecDir(t, dir, 0)
			return
		}
		checked = append(checked, dir)
	}

	t.Skipf("live agency spec dir not found; checked: %v. "+
		"Set INTERCORE_AGENCY_SPEC_DIR to the directory containing the *.yaml stage specs "+
		"(canonical location: os/Clavain/config/agency in the Sylveste monorepo) to run this check. "+
		"Bundled fixtures under %s are still validated by TestBundledSpecFiles.",
		checked, testdataSpecDir)
}
