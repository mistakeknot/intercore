package phase

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/mistakeknot/intercore/pkg/runtimeproof"
)

const runtimeEvidenceRequirement = "runtime-evidence/v1"
const metadataMergeAttempts = 8

var metadataDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// RuntimeEvidenceConfig is the sealed terminal-gate configuration stored in a run.
type RuntimeEvidenceConfig struct {
	Required     bool
	BeadID       string
	Expectations runtimeproof.Expectations
	ConfigDigest string
	Ready        bool
}

// RunConfigUpdate is an atomic run configuration mutation. MetadataMerge is a
// recursive JSON object merge; sealed close-gate fields are monotonic.
type RunConfigUpdate struct {
	Complexity    *int
	AutoAdvance   *bool
	ForceFull     *bool
	MaxDispatches *int
	MetadataMerge string
	beforeCAS     func() // deterministic race injection for package tests
}

// CanonicalMetadata validates a JSON object and returns stable JSON encoding.
func CanonicalMetadata(raw string) (string, error) {
	obj, err := parseMetadataObject(raw)
	if err != nil {
		return "", err
	}
	if err := validateMetadataObject(obj); err != nil {
		return "", err
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return "", fmt.Errorf("%w: encode: %v", ErrInvalidMetadata, err)
	}
	return string(b), nil
}

func canonicalMetadataPointer(raw *string) (*string, error) {
	if raw == nil {
		return nil, nil
	}
	canonical, err := CanonicalMetadata(*raw)
	if err != nil {
		return nil, err
	}
	return &canonical, nil
}

func parseMetadataObject(raw string) (map[string]any, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("%w: empty JSON", ErrInvalidMetadata)
	}
	var obj map[string]any
	dec := json.NewDecoder(bytes.NewBufferString(raw))
	dec.UseNumber()
	if err := dec.Decode(&obj); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidMetadata, err)
	}
	if obj == nil {
		return nil, fmt.Errorf("%w: metadata must be a JSON object", ErrInvalidMetadata)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("%w: multiple JSON values", ErrInvalidMetadata)
		}
		return nil, fmt.Errorf("%w: %v", ErrInvalidMetadata, err)
	}
	return obj, nil
}

// MergeMetadata recursively merges patch into current while preserving sealed
// runtime close-gate activation and provenance.
func MergeMetadata(current, patch string) (string, error) {
	before, err := parseMetadataObject(current)
	if err != nil {
		return "", err
	}
	patchObj, err := parseMetadataObject(patch)
	if err != nil {
		return "", err
	}
	after := cloneObject(before)
	mergeObject(after, patchObj)
	if err := validateSealedCloseGate(before, after); err != nil {
		return "", err
	}
	if err := validateMetadataObject(after); err != nil {
		return "", err
	}
	b, err := json.Marshal(after)
	if err != nil {
		return "", fmt.Errorf("%w: encode: %v", ErrInvalidMetadata, err)
	}
	return string(b), nil
}

func cloneObject(obj map[string]any) map[string]any {
	b, _ := json.Marshal(obj)
	var cloned map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	_ = dec.Decode(&cloned)
	return cloned
}

func mergeObject(dst, patch map[string]any) {
	for key, value := range patch {
		patchMap, patchIsMap := value.(map[string]any)
		currentMap, currentIsMap := dst[key].(map[string]any)
		if patchIsMap && currentIsMap {
			mergeObject(currentMap, patchMap)
			continue
		}
		dst[key] = value
	}
}

func validateMetadataObject(obj map[string]any) error {
	rawCloseGate, exists := obj["close_gate"]
	if !exists || rawCloseGate == nil {
		return nil
	}
	closeGate, ok := rawCloseGate.(map[string]any)
	if !ok {
		return fmt.Errorf("%w: close_gate must be an object", ErrInvalidMetadata)
	}
	requirements, err := requirementSet(closeGate)
	if err != nil {
		return err
	}
	if !requirements[runtimeEvidenceRequirement] {
		return nil
	}
	beadID, ok := closeGate["bead_id"].(string)
	if !ok || strings.TrimSpace(beadID) == "" {
		return fmt.Errorf("%w: runtime evidence requires close_gate.bead_id", ErrInvalidMetadata)
	}
	if adoption, exists := closeGate["adoption"]; exists {
		adoptionObject, ok := adoption.(map[string]any)
		if !ok || len(adoptionObject) == 0 {
			return fmt.Errorf("%w: close_gate.adoption must be an object", ErrInvalidMetadata)
		}
	}
	_, hasExpectations := closeGate["runtime_expectations"]
	_, hasDigest := closeGate["config_digest"]
	if hasExpectations != hasDigest {
		return fmt.Errorf("%w: runtime_expectations and config_digest must be set together", ErrInvalidMetadata)
	}
	if hasExpectations {
		if _, ok := closeGate["runtime_expectations"].(map[string]any); !ok {
			return fmt.Errorf("%w: runtime_expectations must be an object", ErrInvalidMetadata)
		}
		digest, ok := closeGate["config_digest"].(string)
		if !ok || !metadataDigestPattern.MatchString(digest) {
			return fmt.Errorf("%w: config_digest must be a sha256 digest", ErrInvalidMetadata)
		}
		if _, err := decodeRuntimeExpectations(closeGate["runtime_expectations"]); err != nil {
			return err
		}
	}
	return nil
}

func decodeRuntimeExpectations(raw any) (runtimeproof.Expectations, error) {
	b, err := json.Marshal(raw)
	if err != nil {
		return runtimeproof.Expectations{}, fmt.Errorf("%w: encode runtime_expectations: %v", ErrInvalidMetadata, err)
	}
	var expectations runtimeproof.Expectations
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&expectations); err != nil {
		return runtimeproof.Expectations{}, fmt.Errorf("%w: runtime_expectations: %v", ErrInvalidMetadata, err)
	}
	if err := runtimeproof.ValidateExpectations(expectations); err != nil {
		return runtimeproof.Expectations{}, fmt.Errorf("%w: runtime_expectations: %v", ErrInvalidMetadata, err)
	}
	return expectations, nil
}

// RuntimeEvidenceForRun reads the sealed runtime close-gate configuration.
// Required may be true while Ready is false between bind and collection; the
// terminal gate must fail closed in that state.
func RuntimeEvidenceForRun(run *Run) (RuntimeEvidenceConfig, error) {
	if run == nil || run.Metadata == nil {
		return RuntimeEvidenceConfig{}, nil
	}
	obj, err := parseMetadataObject(*run.Metadata)
	if err != nil {
		return RuntimeEvidenceConfig{}, err
	}
	rawCloseGate, ok := obj["close_gate"].(map[string]any)
	if !ok {
		return RuntimeEvidenceConfig{}, nil
	}
	requirements, err := requirementSet(rawCloseGate)
	if err != nil {
		return RuntimeEvidenceConfig{}, err
	}
	if !requirements[runtimeEvidenceRequirement] {
		return RuntimeEvidenceConfig{}, nil
	}
	config := RuntimeEvidenceConfig{Required: true}
	config.BeadID, _ = rawCloseGate["bead_id"].(string)
	rawExpectations, hasExpectations := rawCloseGate["runtime_expectations"]
	digest, hasDigest := rawCloseGate["config_digest"].(string)
	if !hasExpectations || !hasDigest {
		return config, nil
	}
	config.Expectations, err = decodeRuntimeExpectations(rawExpectations)
	if err != nil {
		return config, err
	}
	config.ConfigDigest = digest
	config.Ready = true
	return config, nil
}

func validateRuntimeRun(r *Run, metadata *string) error {
	if metadata == nil {
		return nil
	}
	copyRun := *r
	copyRun.Metadata = metadata
	config, err := RuntimeEvidenceForRun(&copyRun)
	if err != nil {
		return err
	}
	if !config.Required {
		return nil
	}
	if strings.TrimSpace(r.ProjectDir) == "" || !filepath.IsAbs(r.ProjectDir) {
		return fmt.Errorf("%w: runtime evidence requires an absolute project directory", ErrInvalidMetadata)
	}
	chain := ResolveChain(r)
	if r.Phases != nil {
		encoded, _ := json.Marshal(r.Phases)
		if _, err := ParsePhaseChain(string(encoded)); err != nil {
			return fmt.Errorf("%w: runtime evidence requires a valid phase chain: %v", ErrInvalidMetadata, err)
		}
	}
	if len(chain) == 0 || chain[len(chain)-1] != PhaseDone {
		return fmt.Errorf("%w: runtime evidence requires terminal phase %q", ErrInvalidMetadata, PhaseDone)
	}
	return nil
}

func requirementSet(closeGate map[string]any) (map[string]bool, error) {
	result := make(map[string]bool)
	raw, exists := closeGate["requirements"]
	if !exists {
		return result, nil
	}
	values, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("%w: close_gate.requirements must be an array", ErrInvalidMetadata)
	}
	for _, value := range values {
		requirement, ok := value.(string)
		if !ok || requirement == "" || result[requirement] {
			return nil, fmt.Errorf("%w: close_gate.requirements must contain unique non-empty strings", ErrInvalidMetadata)
		}
		result[requirement] = true
	}
	return result, nil
}

func validateSealedCloseGate(before, after map[string]any) error {
	beforeGate, beforeOK := before["close_gate"].(map[string]any)
	if !beforeOK {
		return nil
	}
	beforeRequirements, err := requirementSet(beforeGate)
	if err != nil || !beforeRequirements[runtimeEvidenceRequirement] {
		return err
	}
	afterGate, afterOK := after["close_gate"].(map[string]any)
	if !afterOK {
		return fmt.Errorf("%w: close_gate removed", ErrSealedMetadata)
	}
	afterRequirements, err := requirementSet(afterGate)
	if err != nil {
		return err
	}
	for requirement := range beforeRequirements {
		if !afterRequirements[requirement] {
			return fmt.Errorf("%w: requirement %q removed", ErrSealedMetadata, requirement)
		}
	}
	if afterGate["bead_id"] != beforeGate["bead_id"] {
		return fmt.Errorf("%w: bead_id changed", ErrSealedMetadata)
	}
	for _, field := range []string{"adoption", "runtime_expectations", "config_digest"} {
		beforeValue, existed := beforeGate[field]
		if existed && !reflect.DeepEqual(beforeValue, afterGate[field]) {
			return fmt.Errorf("%w: %s changed", ErrSealedMetadata, field)
		}
	}
	return nil
}

// UpdateConfiguration atomically updates settings, metadata, and the EventSet
// audit record. A raw metadata CAS prevents lost concurrent recursive merges.
func (s *Store) UpdateConfiguration(ctx context.Context, id string, update RunConfigUpdate) error {
	if update.Complexity == nil && update.AutoAdvance == nil && update.ForceFull == nil &&
		update.MaxDispatches == nil && strings.TrimSpace(update.MetadataMerge) == "" {
		return errors.New("run update configuration: no settings to update")
	}
	if update.MetadataMerge != "" {
		if _, err := parseMetadataObject(update.MetadataMerge); err != nil {
			return err
		}
	}

	hookCalled := false
	for attempt := 0; attempt < metadataMergeAttempts; attempt++ {
		currentRun, err := s.Get(ctx, id)
		if err != nil {
			return err
		}
		var current sql.NullString
		if currentRun.Metadata != nil {
			current = sql.NullString{String: *currentRun.Metadata, Valid: true}
		}

		var merged string
		if update.MetadataMerge != "" {
			base := "{}"
			if current.Valid {
				base = current.String
			}
			var err error
			merged, err = MergeMetadata(base, update.MetadataMerge)
			if err != nil {
				return err
			}
			candidate := *currentRun
			candidate.Metadata = &merged
			if err := validateRuntimeRun(&candidate, &merged); err != nil {
				return err
			}
			candidateConfig, err := RuntimeEvidenceForRun(&candidate)
			if err != nil {
				return err
			}
			if candidateConfig.Required && (IsTerminalStatus(currentRun.Status) || ChainIsTerminal(ResolveChain(currentRun), currentRun.Phase)) {
				return fmt.Errorf("%w: cannot activate runtime evidence on a terminal run", ErrInvalidMetadata)
			}
		}
		if update.beforeCAS != nil && !hookCalled {
			hookCalled = true
			update.beforeCAS()
		}

		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("run update configuration: begin: %w", err)
		}
		sets := []string{"updated_at = ?"}
		args := []any{time.Now().Unix()}
		if update.Complexity != nil {
			sets = append(sets, "complexity = ?")
			args = append(args, *update.Complexity)
		}
		if update.AutoAdvance != nil {
			sets = append(sets, "auto_advance = ?")
			args = append(args, boolToInt(*update.AutoAdvance))
		}
		if update.ForceFull != nil {
			sets = append(sets, "force_full = ?")
			args = append(args, boolToInt(*update.ForceFull))
		}
		if update.MaxDispatches != nil {
			sets = append(sets, "max_dispatches = ?")
			args = append(args, *update.MaxDispatches)
		}
		if update.MetadataMerge != "" {
			sets = append(sets, "metadata = ?")
			args = append(args, merged)
		}
		args = append(args, id)
		if current.Valid {
			args = append(args, current.String)
		} else {
			args = append(args, nil)
		}
		args = append(args, currentRun.Phase, currentRun.Status)
		result, err := tx.ExecContext(ctx,
			"UPDATE runs SET "+strings.Join(sets, ", ")+" WHERE id = ? AND metadata IS ? AND phase = ? AND status = ?", args...)
		if err != nil {
			_ = tx.Rollback()
			if isRetryableMetadataError(err) {
				time.Sleep(time.Duration(attempt+1) * time.Millisecond)
				continue
			}
			return fmt.Errorf("run update configuration: update: %w", err)
		}
		rows, err := result.RowsAffected()
		if err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("run update configuration: rows: %w", err)
		}
		if rows == 0 {
			_ = tx.Rollback()
			continue
		}
		var phase string
		if err := tx.QueryRowContext(ctx, `SELECT phase FROM runs WHERE id = ?`, id).Scan(&phase); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("run update configuration: read phase: %w", err)
		}
		if err := s.AddEventQ(ctx, tx, &PhaseEvent{RunID: id, FromPhase: phase, ToPhase: phase, EventType: EventSet}); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("run update configuration: event: %w", err)
		}
		if err := tx.Commit(); err != nil {
			if isRetryableMetadataError(err) {
				time.Sleep(time.Duration(attempt+1) * time.Millisecond)
				continue
			}
			return fmt.Errorf("run update configuration: commit: %w", err)
		}
		return nil
	}
	return ErrStaleMetadata
}

func isRetryableMetadataError(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "database is locked") ||
		strings.Contains(message, "database table is locked") ||
		strings.Contains(message, "busy") ||
		strings.Contains(message, "snapshot")
}
