// Package autonomy resolves the declared human-delegation level and derives
// the run defaults that follow from it.
//
// The delegation ladder (L0-L5) is defined once in the Sylveste canon at
// docs/canon/autonomy.md. It measures how much authority the human delegates
// to agents — a human decision, not an earned property. It is deliberately NOT
// the same scale as the A/B/C roadmap track levels, which are earned by
// observed exit criteria and cannot be set; nor the M0-M4 capability mesh.
//
// The kernel's only concern here is mechanism: store the declared level, and
// derive the run defaults it implies. Deciding *when* to advance the level is
// policy and belongs to the human, informed by evidence the OS aggregates.
package autonomy

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// ConfigKey is the kernel config key holding the declared delegation level.
// Stored via the same "kernel.<key>" state convention as every other
// ic config value, so `ic config set autonomy.delegation_level 2` is the
// single write path.
const ConfigKey = "autonomy.delegation_level"

// StateKey is the fully-qualified state key ConfigKey resolves to.
const StateKey = "kernel." + ConfigKey

// Scope is the state scope kernel config lives under.
const Scope = "global"

// MinLevel and MaxLevel bound the ladder.
const (
	MinLevel = 0
	MaxLevel = 5
)

// DefaultLevel applies when the key has never been set.
//
// It is 2, not 0, because the kernel shipped with AutoAdvance hardcoded true
// at every run-creation site, and that behavior corresponds to L2 ("human
// reviews evidence post-hoc"). Defaulting to anything lower would silently
// start pausing every existing run at every phase boundary on upgrade.
const DefaultLevel = 2

// AutoAdvanceFloor is the lowest level at which runs advance without waiting
// for a human at each phase gate.
//
// L0 ("human approves every action") and L1 ("human approves at phase gates")
// both require the human in the transition. L2 ("human reviews evidence
// post-hoc") is the first level where the run may advance on its own and the
// human catches up afterward.
const AutoAdvanceFloor = 2

// levelNames are the canonical one-line meanings from docs/canon/autonomy.md.
var levelNames = map[int]string{
	0: "human approves every action",
	1: "human approves at phase gates",
	2: "human reviews evidence post-hoc",
	3: "human sets policy, agent executes",
	4: "agent proposes policy changes",
	5: "agent proposes mechanism changes",
}

// Name returns the canonical meaning of a level, or "unknown" if out of range.
func Name(level int) string {
	if n, ok := levelNames[level]; ok {
		return n
	}
	return "unknown"
}

// Validate reports whether level is a real rung on the ladder.
func Validate(level int) error {
	if level < MinLevel || level > MaxLevel {
		return fmt.Errorf("delegation level must be %d-%d, got %d", MinLevel, MaxLevel, level)
	}
	return nil
}

// ParseLevel parses and validates a level from its string form.
func ParseLevel(s string) (int, error) {
	// Accept both "2" and "L2"/"l2" — the canon writes levels with the prefix,
	// so operators reaching for `ic config set ... L2` should not be punished.
	trimmed := strings.TrimSpace(s)
	trimmed = strings.TrimPrefix(strings.TrimPrefix(trimmed, "L"), "l")
	level, err := strconv.Atoi(trimmed)
	if err != nil {
		return 0, fmt.Errorf("delegation level must be an integer %d-%d, got %q", MinLevel, MaxLevel, s)
	}
	if err := Validate(level); err != nil {
		return 0, err
	}
	return level, nil
}

// DerivesAutoAdvance reports the auto_advance default implied by a level.
func DerivesAutoAdvance(level int) bool {
	return level >= AutoAdvanceFloor
}

// Getter reads a state key. Satisfied by internal/state.Store.
type Getter interface {
	Get(ctx context.Context, key, scopeID string) (json.RawMessage, error)
}

// Resolution is the answer to "what level are we at, and where did that come
// from" — the provenance matters because an unset key and an explicitly-set
// L2 produce identical behavior but very different confidence.
type Resolution struct {
	Level       int    `json:"level"`
	Name        string `json:"name"`
	AutoAdvance bool   `json:"derives_auto_advance"`
	Declared    bool   `json:"declared"`
	Source      string `json:"source"`
}

// Resolve reads the declared level from kernel state, falling back to
// DefaultLevel when unset or unparseable.
//
// A malformed stored value falls back rather than erroring: this is consulted
// on the run-creation path, and a typo in a config row should not make the
// kernel unable to create runs. The fallback is visible in Source.
func Resolve(ctx context.Context, g Getter) Resolution {
	fallback := Resolution{
		Level:       DefaultLevel,
		Name:        Name(DefaultLevel),
		AutoAdvance: DerivesAutoAdvance(DefaultLevel),
		Declared:    false,
		Source:      "default (unset)",
	}
	if g == nil {
		return fallback
	}
	payload, err := g.Get(ctx, StateKey, Scope)
	if err != nil || len(payload) == 0 {
		return fallback
	}

	// Values land in the state table as raw JSON, written by `ic config set`.
	// A bare integer is the expected shape; tolerate a quoted string too.
	raw := strings.TrimSpace(string(payload))
	if unquoted, uerr := strconv.Unquote(raw); uerr == nil {
		raw = unquoted
	} else {
		var s string
		if json.Unmarshal(payload, &s) == nil {
			raw = s
		}
	}

	level, perr := ParseLevel(raw)
	if perr != nil {
		fallback.Source = fmt.Sprintf("default (stored value %q invalid)", raw)
		return fallback
	}
	return Resolution{
		Level:       level,
		Name:        Name(level),
		AutoAdvance: DerivesAutoAdvance(level),
		Declared:    true,
		Source:      ConfigKey,
	}
}
