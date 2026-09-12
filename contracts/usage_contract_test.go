package contracts

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestUsageSchemasAreClosedAndCurrent(t *testing.T) {
	dir := t.TempDir()
	if err := GenerateSchemas(dir); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		fields []string
	}{
		{"usage-observation", []string{"id", "provider", "source", "kind", "status", "counters", "identity", "execution_refs", "payload_sha256", "canonical_sha256", "inserted_at"}},
		{"usage-validity", []string{"observation_id", "known_activity", "known_overlap", "external_activity", "binding_coverage", "capture_freshness", "source_freshness", "structure_status", "structure_reasons"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			generated, err := os.ReadFile(filepath.Join(dir, "cli", tc.name+".json"))
			if err != nil {
				t.Fatal(err)
			}
			var schema struct {
				Type                 string                     `json:"type"`
				Properties           map[string]json.RawMessage `json:"properties"`
				AdditionalProperties bool                       `json:"additionalProperties"`
			}
			if err := json.Unmarshal(generated, &schema); err != nil {
				t.Fatal(err)
			}
			if schema.Type != "object" || schema.AdditionalProperties {
				t.Fatalf("schema must be a closed object: %s", generated)
			}
			if tc.name == "usage-observation" {
				var counters struct {
					Items struct {
						Properties map[string]struct {
							OneOf []struct {
								Type string `json:"type"`
							} `json:"oneOf"`
						} `json:"properties"`
					} `json:"items"`
				}
				if err := json.Unmarshal(schema.Properties["counters"], &counters); err != nil {
					t.Fatal(err)
				}
				variants := counters.Items.Properties["value"].OneOf
				if len(variants) != 2 || variants[0].Type != "number" || variants[1].Type != "null" {
					t.Fatalf("counter must remain numeric/null: %s", schema.Properties["counters"])
				}
			}
			for _, field := range tc.fields {
				if _, ok := schema.Properties[field]; !ok {
					t.Errorf("missing property %q", field)
				}
			}
			checkedIn, err := os.ReadFile(filepath.Join("cli", tc.name+".json"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(generated, checkedIn) {
				t.Errorf("%s schema is stale; run go generate ./contracts/...", tc.name)
			}
		})
	}
}
