package contracts

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDispatchSchemaFields(t *testing.T) {
	dir := t.TempDir()
	if err := GenerateSchemas(dir); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "cli", "dispatch.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Type                 string                     `json:"type"`
		Properties           map[string]json.RawMessage `json:"properties"`
		Required             []string                   `json:"required"`
		AdditionalProperties bool                       `json:"additionalProperties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	if schema.Type != "object" || schema.AdditionalProperties {
		t.Fatal("dispatch schema must be a closed object")
	}
	for _, fixture := range []string{"sparse", "populated"} {
		t.Run(fixture, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata", "dispatch-"+fixture+".json"))
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if err := json.Unmarshal(data, &fields); err != nil {
				t.Fatal(err)
			}
			if fixture == "sparse" && len(schema.Required) != len(fields) {
				t.Errorf("required properties = %d, want all %d always-present CLI fields", len(schema.Required), len(fields))
			}
			if fixture == "populated" && len(schema.Properties) != len(fields) {
				t.Errorf("schema properties = %d, want all %d CLI fields", len(schema.Properties), len(fields))
			}
			for _, required := range schema.Required {
				if _, ok := fields[required]; !ok {
					t.Errorf("CLI fixture lacks required schema property %q", required)
				}
			}
			for key, value := range fields {
				propertyData, ok := schema.Properties[key]
				if !ok {
					t.Errorf("CLI field %q has no schema property", key)
					continue
				}
				wantType := ""
				switch value.(type) {
				case string:
					wantType = "string"
				case float64:
					wantType = "integer"
				case map[string]any:
					// Sandbox payloads are embedded JSON, not serialized strings.
					if string(propertyData) != "true" {
						t.Errorf("raw JSON field %q constrained to %s", key, propertyData)
					}
					continue
				default:
					t.Fatalf("unsupported fixture field %q: %T", key, value)
				}
				var property struct {
					Type string `json:"type"`
				}
				if err := json.Unmarshal(propertyData, &property); err != nil {
					t.Fatal(err)
				}
				if property.Type != wantType {
					t.Errorf("field %q type = %s, want %s", key, property.Type, wantType)
				}
			}
		})
	}
	checkedIn, err := os.ReadFile(filepath.Join("cli", "dispatch.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, checkedIn) {
		t.Error("dispatch schema is stale; run go generate ./contracts/...")
	}
}
