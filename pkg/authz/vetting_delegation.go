package authz

import "github.com/mistakeknot/intercore/pkg/autonomy"

// DelegationVettingKey is the reserved key under which an authorization row's
// vetting blob carries what the delegation ceiling did to the decision.
//
// It is a key inside the existing signed `vetting` field rather than new
// columns on the table. The signing spec is explicit that a new signed field
// requires a new sig_version and a parallel signing path
// (see pkg/authz/sign.go, signedFields); unsigned columns would have been the
// cheap alternative, but evidence used to justify raising the delegation level
// is exactly the evidence that must not be silently editable. Putting it in
// `vetting` inherits the existing signature with no cryptographic change.
const DelegationVettingKey = "delegation"

// DelegationEvidence describes the ceiling's effect on one decision.
//
// Provenance is deliberately split. Level/Declared/Source/MinForAuto are facts
// the kernel can re-read for itself at record time, so the recorder resolves
// them rather than trusting whatever the caller echoes back. Capped is not
// re-derivable after the fact — only the evaluation that ran knows whether
// policy would have said `auto` absent the ceiling — so it is supplied by the
// caller that performed the check.
type DelegationEvidence struct {
	Level      int    `json:"level"`
	Name       string `json:"name"`
	Declared   bool   `json:"declared"`
	Source     string `json:"source"`
	MinForAuto int    `json:"min_for_auto"`
	Capped     bool   `json:"capped"`
}

// DelegationVetting renders the reserved subtree for one decision.
//
// It records every decision, not only capped ones. A store holding only
// interventions can say how often the ceiling fired but not how often it could
// have and did not, and "the ceiling has never withheld anything" is only
// meaningful against a denominator.
func DelegationVetting(res autonomy.Resolution, op string, capped bool) map[string]interface{} {
	return map[string]interface{}{
		DelegationVettingKey: map[string]interface{}{
			"level":        res.Level,
			"name":         res.Name,
			"declared":     res.Declared,
			"source":       res.Source,
			"min_for_auto": autonomy.MinLevelForAuto(op),
			"capped":       capped,
		},
	}
}
