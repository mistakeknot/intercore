package publish

import (
	"encoding/json"
	"testing"
)

func TestRigContainsPlugin(t *testing.T) {
	data := []byte(`{"plugins":{"recommended":[{"source":"foo@interagency-marketplace","description":"Foo"}]}}`)
	if !rigContainsPlugin(data, "foo@interagency-marketplace") {
		t.Error("expected to find foo@interagency-marketplace")
	}
	if rigContainsPlugin(data, "bar@interagency-marketplace") {
		t.Error("did not expect to find bar@interagency-marketplace")
	}
}

func TestRigAddRecommended(t *testing.T) {
	input := `{
  "name": "clavain",
  "plugins": {
    "required": [],
    "recommended": [
      {
        "source": "existing@interagency-marketplace",
        "description": "Already here"
      }
    ],
    "optional": []
  }
}`

	result, err := rigAddRecommended([]byte(input), "newplugin@interagency-marketplace", "A new plugin")
	if err != nil {
		t.Fatalf("rigAddRecommended: %v", err)
	}

	// Verify the result is valid JSON
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}

	// Verify the new plugin is present
	if !rigContainsPlugin(result, "newplugin@interagency-marketplace") {
		t.Error("expected new plugin in result")
	}

	// Verify the existing plugin is still present
	if !rigContainsPlugin(result, "existing@interagency-marketplace") {
		t.Error("expected existing plugin still in result")
	}

	// Verify we can find the description
	var full struct {
		Plugins struct {
			Recommended []rigPluginEntry `json:"recommended"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(result, &full); err != nil {
		t.Fatalf("parse result: %v", err)
	}

	if len(full.Plugins.Recommended) != 2 {
		t.Fatalf("expected 2 recommended, got %d", len(full.Plugins.Recommended))
	}

	last := full.Plugins.Recommended[1]
	if last.Source != "newplugin@interagency-marketplace" {
		t.Errorf("expected source newplugin@interagency-marketplace, got %s", last.Source)
	}
	if last.Description != "A new plugin" {
		t.Errorf("expected description 'A new plugin', got %s", last.Description)
	}
}

func TestRigAddRecommendedIdempotent(t *testing.T) {
	// If plugin is already in the JSON, rigContainsPlugin should catch it
	// before rigAddRecommended is ever called. But test the detection.
	input := `{"plugins":{"recommended":[{"source":"foo@interagency-marketplace","description":"Foo"}]}}`

	if !rigContainsPlugin([]byte(input), "foo@interagency-marketplace") {
		t.Error("expected rigContainsPlugin to return true for existing plugin")
	}
}

func TestRigAddRecommendedPreservesOtherFields(t *testing.T) {
	input := `{
  "name": "clavain",
  "version": "0.6.161",
  "plugins": {
    "recommended": []
  },
  "mcpServers": {"context7": {}}
}`

	result, err := rigAddRecommended([]byte(input), "test@interagency-marketplace", "Test")
	if err != nil {
		t.Fatalf("rigAddRecommended: %v", err)
	}

	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}

	// Verify other top-level fields are preserved
	for _, key := range []string{"name", "version", "mcpServers"} {
		if _, ok := parsed[key]; !ok {
			t.Errorf("expected key %q to be preserved", key)
		}
	}
}
