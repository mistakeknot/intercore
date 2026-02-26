package contracts

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/invopop/jsonschema"
)

// GenerateSchemas generates JSON Schema files for all registered contract types.
// Output is written to subdirectories cli/ and events/ under outDir.
func GenerateSchemas(outDir string) error {
	cliDir := filepath.Join(outDir, "cli")
	eventsDir := filepath.Join(outDir, "events")

	if err := os.MkdirAll(cliDir, 0755); err != nil {
		return fmt.Errorf("mkdir cli: %w", err)
	}
	if err := os.MkdirAll(eventsDir, 0755); err != nil {
		return fmt.Errorf("mkdir events: %w", err)
	}

	r := &jsonschema.Reflector{
		DoNotReference: true,
	}

	for _, ct := range CLIContracts {
		schema := r.Reflect(ct.Instance)
		schema.ID = jsonschema.ID(fmt.Sprintf("https://intercore.dev/contracts/cli/%s.json", ct.Name))

		data, err := json.MarshalIndent(schema, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal %s: %w", ct.Name, err)
		}
		path := filepath.Join(cliDir, ct.Name+".json")
		if err := os.WriteFile(path, append(data, '\n'), 0644); err != nil {
			return fmt.Errorf("write %s: %w", ct.Name, err)
		}
	}

	for _, ct := range EventContracts {
		schema := r.Reflect(ct.Instance)
		schema.ID = jsonschema.ID(fmt.Sprintf("https://intercore.dev/contracts/events/%s.json", ct.Name))

		data, err := json.MarshalIndent(schema, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal %s: %w", ct.Name, err)
		}
		path := filepath.Join(eventsDir, ct.Name+".json")
		if err := os.WriteFile(path, append(data, '\n'), 0644); err != nil {
			return fmt.Errorf("write %s: %w", ct.Name, err)
		}
	}

	return nil
}
