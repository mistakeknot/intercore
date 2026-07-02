package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/mistakeknot/intercore/internal/phase"
)

// CalibratedTierEntry represents one entry in the calibration JSON file.
type CalibratedTierEntry struct {
	Tier      string  `json:"tier"`       // "hard" or "soft"
	Locked    bool    `json:"locked"`     // if true, skip (fall through to hardcoded defaults)
	FPR       float64 `json:"fpr"`        // false positive rate
	WeightedN float64 `json:"weighted_n"` // effective sample size after decay
	UpdatedAt int64   `json:"updated_at"` // unix timestamp of last calibration
}

// CalibratedTiersFile is the top-level structure of a gate-tier-calibration.json file.
type CalibratedTiersFile struct {
	Tiers     map[string]CalibratedTierEntry `json:"tiers"`      // keyed by GateCalibrationKey
	CreatedAt int64                          `json:"created_at"` // unix timestamp of file creation
}

const calibrationStalenessDuration = 24 * time.Hour

// calibrationStaleDetected is set by LoadGateCalibration when a stale file is
// encountered. Callers should check this after LoadGateCalibration returns and
// emit an observability event if needed.
var calibrationStaleDetected bool

// emitCalibrationStaleEvent records the staleness as an interspect event.
// Best-effort: failures are logged but do not block the caller.
func emitCalibrationStaleEvent(path string) {
	if !calibrationStaleDetected {
		return
	}
	calibrationStaleDetected = false
	detail := fmt.Sprintf(`{"event_type":"calibration_file_stale","path":%q}`, path)
	code := cmdEventsRecord(context.Background(), []string{
		"--source=interspect",
		"--type=calibration_file_stale",
		"--payload=" + detail,
	})
	if code != 0 {
		slog.Warn("failed to emit calibration_file_stale event", "code", code)
	}
}

// LoadGateCalibration reads and validates a calibration JSON file, returning
// a map of GateCalibrationKey → tier suitable for GateConfig.CalibratedTiers.
//
// Validation:
//   - Staleness: file older than 24h → treat as absent (log warning, return nil)
//   - Locked entries: skip (fall through to hardcoded defaults)
//   - Promotion-only: skip entries where file says "soft" but hardcoded default is "hard"
//     (calibration can only escalate soft→hard, not demote hard→soft)
func LoadGateCalibration(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("load gate calibration: %w", err)
	}

	var file CalibratedTiersFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("load gate calibration: parse: %w", err)
	}

	// Staleness check: treat files older than 24h as absent
	age := time.Since(time.Unix(file.CreatedAt, 0))
	if age > calibrationStalenessDuration {
		slog.Warn("gate calibration file is stale, ignoring",
			"path", path,
			"age", age.Round(time.Minute).String(),
			"threshold", calibrationStalenessDuration.String(),
		)
		calibrationStaleDetected = true
		return nil, nil
	}

	if len(file.Tiers) == 0 {
		return nil, nil
	}

	// Build the hardcoded defaults map for promotion-only enforcement
	hardcodedDefaults := buildHardcodedDefaultTiers()

	result := make(map[string]string, len(file.Tiers))
	for key, entry := range file.Tiers {
		// Skip locked entries — fall through to hardcoded defaults
		if entry.Locked {
			continue
		}

		// Promotion-only enforcement: calibration can escalate soft→hard,
		// but never demote hard→soft. If the hardcoded default is already "hard",
		// skip any calibrated "soft" entry.
		if entry.Tier == phase.TierSoft {
			if defaultTier, ok := hardcodedDefaults[key]; ok && defaultTier == phase.TierHard {
				continue
			}
		}

		if entry.Tier != phase.TierHard && entry.Tier != phase.TierSoft {
			slog.Warn("gate calibration: invalid tier, skipping", "key", key, "tier", entry.Tier)
			continue
		}

		result[key] = entry.Tier
	}

	if len(result) == 0 {
		return nil, nil
	}
	return result, nil
}

// LoadGateCalibrationRaw reads the calibration JSON file and returns the raw
// entries for output enrichment. Returns nil if file is missing, corrupt, or stale.
func LoadGateCalibrationRaw(path string) map[string]CalibratedTierEntry {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var file CalibratedTiersFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil
	}
	age := time.Since(time.Unix(file.CreatedAt, 0))
	if age > calibrationStalenessDuration {
		return nil
	}
	return file.Tiers
}

// buildHardcodedDefaultTiers returns the tier for each check in the hardcoded
// gateRules table. Since hardcoded rules don't specify per-rule tiers (they
// inherit from priority), the "default" tier is effectively TierSoft for all
// hardcoded transitions. This means calibration can promote any default to hard.
func buildHardcodedDefaultTiers() map[string]string {
	defaults := make(map[string]string)
	// Hardcoded gate rules have no per-rule tier — they use cfg.Priority.
	// For promotion-only purposes, we treat missing tier as "soft" (the least
	// restrictive), since calibration's job is to escalate to hard.
	// If any hardcoded rule specifies tier=hard, we record that.
	for key, rules := range phase.GateRulesForCalibration() {
		for _, r := range rules {
			calKey := phase.GateCalibrationKey(r.Check, key[0], key[1])
			if r.Tier == phase.TierHard {
				defaults[calKey] = phase.TierHard
			} else {
				defaults[calKey] = phase.TierSoft
			}
		}
	}
	return defaults
}
