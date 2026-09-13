package routing

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/big"
	"os"
	"strconv"
	"strings"
)

const calibrationArtifactKind = "agent_model_calibration"

// CalibrationExclusion explains why a candidate or artifact was not used.
type CalibrationExclusion struct {
	Scope  string `json:"scope"`
	Reason string `json:"reason"`
}

// CalibrationMetadata makes calibration consumption explicit without implying
// admission, promotion, freshness, or host qualification.
type CalibrationMetadata struct {
	ArtifactKind     string                 `json:"artifact_kind"`
	SHA256           *string                `json:"sha256"`
	SchemaVersion    *int                   `json:"schema_version"`
	Mode             string                 `json:"mode"`
	ModeSource       string                 `json:"mode_source"`
	Disposition      string                 `json:"disposition"`
	RecommendedModel *string                `json:"recommended_model"`
	SelectedScope    *string                `json:"selected_scope"`
	SelectedArm      *string                `json:"selected_arm"`
	Exclusions       []CalibrationExclusion `json:"exclusions"`
	FallbackReason   *string                `json:"fallback_reason"`
	Freshness        string                 `json:"freshness"`
	FreshnessReason  string                 `json:"freshness_reason"`
	Authority        string                 `json:"authority"`
}

// ModelResolution is the additive detailed form of ResolveModel.
type ModelResolution struct {
	Model       string               `json:"model"`
	Calibration *CalibrationMetadata `json:"calibration,omitempty"`
}

// BatchModelResolution keeps legacy model keys together and attaches
// per-agent diagnostics only when an artifact was explicitly supplied.
type BatchModelResolution struct {
	Models      map[string]string              `json:"models"`
	Calibration map[string]CalibrationMetadata `json:"calibration"`
}

type calibrationEntry struct {
	recommendedModel    *string
	confidence          *big.Rat
	evidenceSessions    *big.Rat
	propagationEligible *bool
	phases              map[string]calibrationEntry
}

// CalibrationArtifact is an immutable parse of one exact file read. Parse and
// read failures are retained as metadata so routing can fail back to static.
type CalibrationArtifact struct {
	metadata CalibrationMetadata
	agents   map[string]calibrationEntry
	valid    bool
}

type calibrationParseError struct {
	reason string
	err    error
}

func (e *calibrationParseError) Error() string { return e.err.Error() }

func calibrationFailure(reason string, format string, args ...any) error {
	return &calibrationParseError{reason: reason, err: fmt.Errorf(format, args...)}
}

// ReadCalibration reads and hashes path exactly once. It never changes routing
// on failure; callers receive explicit static diagnostics instead.
func ReadCalibration(path string) *CalibrationArtifact {
	a := &CalibrationArtifact{metadata: CalibrationMetadata{
		ArtifactKind:    calibrationArtifactKind,
		Disposition:     "static",
		Exclusions:      []CalibrationExclusion{},
		Freshness:       "unknown",
		FreshnessReason: "artifact_has_no_verified_validity",
		Authority:       "resolution_only",
	}}
	data, err := os.ReadFile(path)
	if err != nil {
		a.metadata.Exclusions = append(a.metadata.Exclusions, CalibrationExclusion{Scope: "artifact", Reason: "artifact_unreadable"})
		a.metadata.FallbackReason = stringPointer("calibration_artifact_unreadable")
		return a
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	a.metadata.SHA256 = &digest

	root, err := decodeStrictJSON(data)
	if err != nil {
		a.invalidate(parseReason(err))
		return a
	}
	obj, ok := root.(map[string]any)
	if !ok {
		a.invalidate("top_level_not_object")
		return a
	}
	schema, err := requiredSchema(obj["schema_version"])
	if err != nil {
		a.invalidate(parseReason(err))
		return a
	}
	a.metadata.SchemaVersion = &schema
	if schema < 1 || schema > 3 {
		a.invalidate("unsupported_schema")
		return a
	}

	a.agents = map[string]calibrationEntry{}
	if rawAgents, exists := obj["agents"]; exists {
		agents, ok := rawAgents.(map[string]any)
		if !ok {
			a.invalidate("agents_wrong_type")
			return a
		}
		for agent, rawEntry := range agents {
			entryObj, ok := rawEntry.(map[string]any)
			if !ok {
				a.invalidate("agent_entry_wrong_type")
				return a
			}
			entry, err := parseCalibrationEntry(entryObj, true)
			if err != nil {
				a.invalidate(parseReason(err))
				return a
			}
			a.agents[agent] = entry
		}
	}
	a.valid = true
	return a
}

func (a *CalibrationArtifact) invalidate(reason string) {
	a.metadata.Exclusions = append(a.metadata.Exclusions, CalibrationExclusion{Scope: "artifact", Reason: reason})
	a.metadata.FallbackReason = stringPointer("invalid_calibration_artifact")
}

func parseReason(err error) string {
	if parseErr, ok := err.(*calibrationParseError); ok {
		return parseErr.reason
	}
	return "invalid_json"
}

func requiredSchema(value any) (int, error) {
	f, err := exactCalibrationNumber(value, "schema_wrong_type")
	if err != nil {
		return 0, err
	}
	if !f.IsInt() || !f.Num().IsInt64() || int64(int(f.Num().Int64())) != f.Num().Int64() {
		return 0, calibrationFailure("schema_wrong_type", "schema_version must be an integral finite number")
	}
	return int(f.Num().Int64()), nil
}

func parseCalibrationEntry(obj map[string]any, allowPhases bool) (calibrationEntry, error) {
	entry := calibrationEntry{phases: map[string]calibrationEntry{}}
	if value, ok := obj["recommended_model"]; ok {
		model, ok := value.(string)
		if !ok {
			return entry, calibrationFailure("recommended_model_wrong_type", "recommended_model must be a string")
		}
		entry.recommendedModel = &model
	}
	if value, ok := obj["confidence"]; ok {
		confidence, err := exactCalibrationNumber(value, "confidence_wrong_type")
		if err != nil {
			return entry, err
		}
		entry.confidence = confidence
	}
	if value, ok := obj["evidence_sessions"]; ok {
		sessions, err := exactCalibrationNumber(value, "evidence_sessions_wrong_type")
		if err != nil {
			return entry, err
		}
		entry.evidenceSessions = sessions
	}
	if value, ok := obj["propagation_eligible"]; ok {
		eligible, ok := value.(bool)
		if !ok {
			return entry, calibrationFailure("propagation_eligible_wrong_type", "propagation_eligible must be a boolean")
		}
		entry.propagationEligible = &eligible
	}
	if value, ok := obj["phases"]; ok {
		if !allowPhases {
			return entry, calibrationFailure("phases_wrong_type", "nested phases are not supported")
		}
		phases, ok := value.(map[string]any)
		if !ok {
			return entry, calibrationFailure("phases_wrong_type", "phases must be an object")
		}
		for phase, rawPhase := range phases {
			phaseObj, ok := rawPhase.(map[string]any)
			if !ok {
				return entry, calibrationFailure("phase_entry_wrong_type", "phase %q must be an object", phase)
			}
			parsed, err := parseCalibrationEntry(phaseObj, false)
			if err != nil {
				return entry, err
			}
			entry.phases[phase] = parsed
		}
	}
	return entry, nil
}

func finiteNumber(value any, wrongTypeReason string) (float64, error) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, calibrationFailure(wrongTypeReason, "field must be a number")
	}
	f, err := strconv.ParseFloat(number.String(), 64)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return 0, calibrationFailure("nonfinite_number", "number must be finite")
	}
	return f, nil
}

// Compare authority thresholds against the exact JSON decimal. Float rounding
// must not turn a fractional session count or sub-threshold confidence eligible.
func exactCalibrationNumber(value any, wrongTypeReason string) (*big.Rat, error) {
	f, err := finiteNumber(value, wrongTypeReason)
	if err != nil {
		return nil, err
	}
	raw := value.(json.Number).String()
	if f == 0 {
		mantissa := raw
		if i := strings.IndexAny(raw, "eE"); i >= 0 {
			mantissa = raw[:i]
		}
		if strings.ContainsAny(mantissa, "123456789") {
			return nil, calibrationFailure("numeric_underflow", "number is below the supported finite range")
		}
		// Avoid constructing enormous denominators for zero with a large exponent.
		return big.NewRat(0, 1), nil
	}
	number, ok := new(big.Rat).SetString(raw)
	if !ok {
		return nil, calibrationFailure(wrongTypeReason, "field must be an exact finite number")
	}
	return number, nil
}

func decodeStrictJSON(data []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, err := decodeJSONValue(decoder)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return nil, calibrationFailure("trailing_json", "trailing JSON value")
		}
		return nil, calibrationFailure("invalid_json", "trailing JSON: %v", err)
	}
	return value, nil
}

func decodeJSONValue(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, calibrationFailure("invalid_json", "decode JSON: %v", err)
	}
	switch value := token.(type) {
	case json.Delim:
		switch value {
		case '{':
			obj := map[string]any{}
			seen := map[string]bool{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return nil, calibrationFailure("invalid_json", "decode object key: %v", err)
				}
				key, ok := keyToken.(string)
				if !ok {
					return nil, calibrationFailure("invalid_json", "object key is not a string")
				}
				if seen[key] {
					return nil, calibrationFailure("duplicate_key", "duplicate key %q", key)
				}
				seen[key] = true
				child, err := decodeJSONValue(decoder)
				if err != nil {
					return nil, err
				}
				obj[key] = child
			}
			if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
				return nil, calibrationFailure("invalid_json", "unterminated object")
			}
			return obj, nil
		case '[':
			items := []any{}
			for decoder.More() {
				child, err := decodeJSONValue(decoder)
				if err != nil {
					return nil, err
				}
				items = append(items, child)
			}
			if end, err := decoder.Token(); err != nil || end != json.Delim(']') {
				return nil, calibrationFailure("invalid_json", "unterminated array")
			}
			return items, nil
		default:
			return nil, calibrationFailure("invalid_json", "unexpected delimiter")
		}
	case json.Number:
		if _, err := finiteNumber(value, "nonfinite_number"); err != nil {
			return nil, err
		}
	}
	return token, nil
}

func (a *CalibrationArtifact) metadataCopy() CalibrationMetadata {
	metadata := a.metadata
	metadata.Exclusions = append([]CalibrationExclusion{}, a.metadata.Exclusions...)
	return metadata
}

func calibrationPhaseKeys(phase string) []string {
	if phase == "" {
		return nil
	}
	groups := [][]string{
		{"ship", "shipping", "quality-gates", "quality_gates"},
		{"plan", "planning"},
		{"build", "implementation", "implement"},
	}
	keys := []string{phase}
	for _, group := range groups {
		found := false
		for _, candidate := range group {
			if candidate == phase {
				found = true
				break
			}
		}
		if found {
			for _, candidate := range group {
				if candidate != phase {
					keys = append(keys, candidate)
				}
			}
			break
		}
	}
	return keys
}

func normalizedCalibrationAgent(agent string) string {
	if index := strings.LastIndex(agent, ":"); index >= 0 {
		return agent[index+1:]
	}
	return agent
}

func entryEligibility(entry calibrationEntry, schema int) (string, []string) {
	reasons := []string{}
	if entry.recommendedModel == nil {
		reasons = append(reasons, "missing_recommended_model")
	} else if *entry.recommendedModel != "haiku" && *entry.recommendedModel != "sonnet" && *entry.recommendedModel != "opus" {
		reasons = append(reasons, "unsupported_model")
	}
	if entry.confidence == nil {
		reasons = append(reasons, "missing_confidence")
	} else {
		if entry.confidence.Cmp(big.NewRat(7, 10)) < 0 {
			reasons = append(reasons, "confidence_below_threshold")
		}
		if entry.confidence.Cmp(big.NewRat(1, 1)) > 0 {
			reasons = append(reasons, "confidence_above_maximum")
		}
	}
	if entry.evidenceSessions == nil {
		reasons = append(reasons, "missing_evidence_sessions")
	} else {
		if !entry.evidenceSessions.IsInt() {
			reasons = append(reasons, "evidence_sessions_not_integral")
		}
		if entry.evidenceSessions.Cmp(big.NewRat(3, 1)) < 0 {
			reasons = append(reasons, "insufficient_evidence_sessions")
		}
	}
	if schema >= 2 {
		if entry.propagationEligible == nil {
			reasons = append(reasons, "missing_propagation_eligibility")
		} else if !*entry.propagationEligible {
			reasons = append(reasons, "propagation_ineligible")
		}
	}
	if len(reasons) > 0 || entry.recommendedModel == nil {
		return "", reasons
	}
	return *entry.recommendedModel, reasons
}

func (a *CalibrationArtifact) recommendation(opts ResolveOpts, metadata *CalibrationMetadata) (string, bool) {
	if !a.valid || a.metadata.SchemaVersion == nil {
		return "", false
	}
	agent := normalizedCalibrationAgent(opts.Agent)
	if agent == "" {
		metadata.Exclusions = append(metadata.Exclusions, CalibrationExclusion{Scope: "agent", Reason: "agent_missing"})
		metadata.FallbackReason = stringPointer("agent_missing")
		return "", false
	}
	entry, ok := a.agents[agent]
	if !ok {
		metadata.Exclusions = append(metadata.Exclusions, CalibrationExclusion{Scope: "agent", Reason: "agent_not_found"})
		metadata.FallbackReason = stringPointer("agent_not_found")
		return "", false
	}
	phaseRequested := opts.Phase != ""
	for _, phase := range calibrationPhaseKeys(opts.Phase) {
		phaseEntry, exists := entry.phases[phase]
		if !exists {
			metadata.Exclusions = append(metadata.Exclusions, CalibrationExclusion{Scope: "phase:" + phase, Reason: "missing_phase_entry"})
			continue
		}
		model, reasons := entryEligibility(phaseEntry, *a.metadata.SchemaVersion)
		for _, reason := range reasons {
			metadata.Exclusions = append(metadata.Exclusions, CalibrationExclusion{Scope: "phase:" + phase, Reason: reason})
		}
		if model != "" {
			metadata.RecommendedModel = stringPointer(model)
			metadata.SelectedScope = stringPointer("phase:" + phase)
			return model, true
		}
	}
	model, reasons := entryEligibility(entry, *a.metadata.SchemaVersion)
	for _, reason := range reasons {
		metadata.Exclusions = append(metadata.Exclusions, CalibrationExclusion{Scope: "agent", Reason: reason})
	}
	if model == "" {
		metadata.FallbackReason = stringPointer("no_eligible_recommendation")
		return "", false
	}
	metadata.RecommendedModel = stringPointer(model)
	metadata.SelectedScope = stringPointer("agent")
	if phaseRequested {
		metadata.FallbackReason = stringPointer("phase_recommendation_unavailable")
	}
	return model, true
}

func stringPointer(value string) *string { return &value }
