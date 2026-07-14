package routing

import "fmt"

// This file implements trust-zone constraint enforcement from the
// routing-contract spec (§1 constraints block, Q-1.4 flat match list,
// Q-1.6 constraints are always safety-class / fail-closed).
//
// The DoD's first clause lives here: a client-confidential task must route
// ONLY to a local or approved-vendor deployment, and be BLOCKED otherwise.
// Enforcement is fail-closed: a constraint whose required trust zones cannot
// be satisfied is a hard block (typed error), never a silent downgrade.

// Constraint is one flat match rule (Q-1.4). If every key/value in Match is
// present in the task descriptor's fields, RequireTrustZone applies: the
// chosen deployment's trust zone MUST be in the allowed set.
type Constraint struct {
	Match            map[string]string `yaml:"if"`
	RequireTrustZone []TrustZone       `yaml:"-"`
}

// rawConstraint mirrors the on-disk shape: `require: {trust_zone: [...]}`.
type rawConstraint struct {
	Match   map[string]string `yaml:"if"`
	Require struct {
		TrustZone []TrustZone `yaml:"trust_zone"`
	} `yaml:"require"`
}

// UnmarshalYAML flattens the nested require.trust_zone into the Constraint.
func (c *Constraint) UnmarshalYAML(unmarshal func(any) error) error {
	var raw rawConstraint
	if err := unmarshal(&raw); err != nil {
		return err
	}
	c.Match = raw.Match
	c.RequireTrustZone = raw.Require.TrustZone
	return nil
}

// TaskDescriptor carries the caller-supplied fields a constraint matches on.
// Fields is the open string map (e.g. {"data": "client-confidential"}).
type TaskDescriptor struct {
	Role   string
	Class  string
	Fields map[string]string
}

// ConstraintError is the typed block returned when no eligible deployment
// satisfies an applicable constraint. It is a hard failure (fail-closed),
// distinct from "no model matched" — the caller MUST halt, never fall back.
type ConstraintError struct {
	Field    string      // the descriptor field that triggered the constraint
	Value    string      // its value (e.g. "client-confidential")
	Required []TrustZone // the zones that would have been acceptable
	Got      TrustZone   // the zone of the deployment that was blocked (if any)
}

func (e *ConstraintError) Error() string {
	return fmt.Sprintf("constraint block: %s=%q requires trust_zone in %v (candidate zone %q not permitted)",
		e.Field, e.Value, e.Required, e.Got)
}

// applies reports whether this constraint's Match is fully satisfied by the
// descriptor's fields (all-match, per Q-1.4).
func (c Constraint) applies(td TaskDescriptor) bool {
	for k, want := range c.Match {
		if td.Fields[k] != want {
			return false
		}
	}
	return len(c.Match) > 0 // an empty match never applies
}

// permits reports whether a deployment's trust zone satisfies this constraint.
func (c Constraint) permits(zone TrustZone) bool {
	for _, allowed := range c.RequireTrustZone {
		if zone == allowed {
			return true
		}
	}
	return false
}

// firstMatchField returns a representative (field, value) for error reporting.
func (c Constraint) firstMatchField() (string, string) {
	for k, v := range c.Match {
		return k, v
	}
	return "", ""
}

// CheckConstraints enforces all applicable constraints against a candidate
// deployment zone. Returns a *ConstraintError (fail-closed) on the first
// violation, nil if every applicable constraint permits the zone.
//
// This is the enforcement point behind the DoD's first clause.
func CheckConstraints(constraints []Constraint, td TaskDescriptor, candidateZone TrustZone) error {
	for _, c := range constraints {
		if !c.applies(td) {
			continue
		}
		if !c.permits(candidateZone) {
			field, value := c.firstMatchField()
			return &ConstraintError{
				Field:    field,
				Value:    value,
				Required: c.RequireTrustZone,
				Got:      candidateZone,
			}
		}
	}
	return nil
}

// EligibleDeployments filters a registry to the deployment keys whose trust
// zone satisfies every applicable constraint for this descriptor. An empty
// result means every deployment was blocked — the caller must halt.
func (r *Registry) EligibleDeployments(constraints []Constraint, td TaskDescriptor) []string {
	var eligible []string
	for key, dep := range r.Deployments {
		if CheckConstraints(constraints, td, dep.TrustZone) == nil {
			eligible = append(eligible, key)
		}
	}
	return eligible
}
