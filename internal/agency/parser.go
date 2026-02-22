package agency

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// ParseFile reads a YAML agency spec file and returns a parsed Spec.
func ParseFile(path string) (*Spec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("agency: read %s: %w", path, err)
	}
	spec, err := ParseBytes(data)
	if err != nil {
		return nil, fmt.Errorf("agency: parse %s: %w", path, err)
	}
	return spec, nil
}

// ParseBytes parses raw YAML bytes into a Spec.
func ParseBytes(data []byte) (*Spec, error) {
	var spec Spec
	if err := yaml.Unmarshal(data, &spec); err != nil {
		return nil, fmt.Errorf("agency: unmarshal: %w", err)
	}
	return &spec, nil
}
