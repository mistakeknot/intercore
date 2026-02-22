package agency

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultSpecFiles(t *testing.T) {
	specDir := "../../../../hub/clavain/config/agency"
	if _, err := os.Stat(specDir); os.IsNotExist(err) {
		t.Skip("spec dir not found (run from intercore root)")
	}

	files, err := filepath.Glob(filepath.Join(specDir, "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 5 {
		t.Fatalf("expected 5 spec files, got %d", len(files))
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
