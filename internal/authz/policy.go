package authz

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

const (
	ModeAuto      = "auto"
	ModeConfirm   = "confirm"
	ModeBlock     = "block"
	ModeForceAuto = "force_auto"
)

// Policy is an authorization policy document.
type Policy struct {
	Version int    `yaml:"version" json:"version"`
	Rules   []Rule `yaml:"rules" json:"rules"`
}

// Rule defines behavior for a given operation selector.
type Rule struct {
	Op            string                 `yaml:"op" json:"op"`
	Mode          string                 `yaml:"mode" json:"mode"`
	Requires      map[string]interface{} `yaml:"requires,omitempty" json:"requires,omitempty"`
	AllowOverride bool                   `yaml:"allow_override,omitempty" json:"allow_override,omitempty"`
}

// CheckResult returns the decision from Check.
type CheckResult struct {
	Mode        string `json:"mode"`
	PolicyMatch string `json:"policy_match"`
	PolicyHash  string `json:"policy_hash"`
	Reason      string `json:"reason"`
}

// LoadPolicy loads a policy from YAML file.
func LoadPolicy(path string) (*Policy, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read policy %q: %w", path, err)
	}
	var p Policy
	if err := yaml.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parse policy %q: %w", path, err)
	}
	if p.Version == 0 {
		p.Version = 1
	}
	for i := range p.Rules {
		if p.Rules[i].Requires == nil {
			p.Rules[i].Requires = map[string]interface{}{}
		}
	}
	return &p, nil
}

// LoadEffective loads and merges global, project, and env policies (in that order),
// returning the merged policy and a sha256 hash of its canonical JSON encoding.
func LoadEffective(globalPath, projectPath, envPath string) (*Policy, string, error) {
	global, err := LoadPolicy(globalPath)
	if err != nil {
		return nil, "", err
	}
	project, err := LoadPolicy(projectPath)
	if err != nil {
		return nil, "", err
	}
	env, err := LoadPolicy(envPath)
	if err != nil {
		return nil, "", err
	}

	merged, err := MergePolicies(global, project, env)
	if err != nil {
		return nil, "", err
	}

	encoded, err := canonicalPolicyJSON(merged)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(encoded)
	return merged, hex.EncodeToString(sum[:]), nil
}

func canonicalPolicyJSON(policy *Policy) ([]byte, error) {
	if policy == nil {
		return nil, fmt.Errorf("nil policy")
	}
	return json.Marshal(policy)
}
