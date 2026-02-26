package contracts

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestGenerateSchemas_ProducesFiles(t *testing.T) {
	dir := t.TempDir()
	err := GenerateSchemas(dir)
	if err != nil {
		t.Fatalf("GenerateSchemas: %v", err)
	}

	cliDir := filepath.Join(dir, "cli")
	entries, err := os.ReadDir(cliDir)
	if err != nil {
		t.Fatalf("read cli dir: %v", err)
	}
	if len(entries) == 0 {
		t.Error("no CLI schemas generated")
	}
	if len(entries) != len(CLIContracts) {
		t.Errorf("expected %d CLI schemas, got %d", len(CLIContracts), len(entries))
	}

	eventsDir := filepath.Join(dir, "events")
	entries, err = os.ReadDir(eventsDir)
	if err != nil {
		t.Fatalf("read events dir: %v", err)
	}
	if len(entries) == 0 {
		t.Error("no event schemas generated")
	}
	if len(entries) != len(EventContracts) {
		t.Errorf("expected %d event schemas, got %d", len(EventContracts), len(entries))
	}
}

func TestGenerateSchemas_ValidJSON(t *testing.T) {
	dir := t.TempDir()
	if err := GenerateSchemas(dir); err != nil {
		t.Fatalf("GenerateSchemas: %v", err)
	}

	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || filepath.Ext(path) != ".json" {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if len(data) == 0 {
			t.Errorf("empty schema: %s", path)
		}
		if !bytes.Contains(data, []byte(`"$schema"`)) {
			t.Errorf("missing $schema in: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
