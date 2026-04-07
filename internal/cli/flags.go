// Package cli provides shared flag parsing utilities for ic subcommands.
//
// The parser handles --key=value, --key value, and bare --flag patterns,
// collecting non-flag arguments as positionals.
package cli

import (
	"fmt"
	"strconv"
	"time"
)

// Flags holds parsed flag values and positional arguments.
type Flags struct {
	values      map[string]string
	bools       map[string]bool
	Positionals []string
}

// ParseFlags parses a slice of CLI arguments into structured flags.
// It recognizes two forms:
//   - --key=value  (value flag with = separator)
//   - --key        (boolean flag)
//
// Arguments that don't start with -- are collected as positionals.
// Short flags (-f) are treated as positionals for subcommands that handle
// them inline (e.g., -f as alias for --follow).
func ParseFlags(args []string) *Flags {
	f := &Flags{
		values: make(map[string]string),
		bools:  make(map[string]bool),
	}

	for i := 0; i < len(args); i++ {
		arg := args[i]

		if len(arg) < 3 || arg[:2] != "--" {
			f.Positionals = append(f.Positionals, arg)
			continue
		}

		// Strip leading --
		key := arg[2:]

		// --key=value form
		if eqIdx := indexOf(key, '='); eqIdx >= 0 {
			f.values[key[:eqIdx]] = key[eqIdx+1:]
			continue
		}

		// Bare --flag (boolean)
		f.bools[key] = true
	}

	return f
}

// indexOf returns the index of byte c in s, or -1.
func indexOf(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// Has returns true if the flag was provided (as value or boolean).
func (f *Flags) Has(name string) bool {
	if _, ok := f.values[name]; ok {
		return true
	}
	return f.bools[name]
}

// String returns the value for a string flag, or defaultVal if not set.
func (f *Flags) String(name string, defaultVal string) string {
	if v, ok := f.values[name]; ok {
		return v
	}
	return defaultVal
}

// StringPtr returns a pointer to the flag value, or nil if not set.
func (f *Flags) StringPtr(name string) *string {
	if v, ok := f.values[name]; ok {
		return &v
	}
	return nil
}

// Int returns the integer value for a flag, or defaultVal if not set.
// Returns an error if the value is present but not a valid integer.
func (f *Flags) Int(name string, defaultVal int) (int, error) {
	v, ok := f.values[name]
	if !ok {
		return defaultVal, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("invalid value for --%s: %q", name, v)
	}
	return n, nil
}

// Int64 returns the int64 value for a flag, or defaultVal if not set.
func (f *Flags) Int64(name string, defaultVal int64) (int64, error) {
	v, ok := f.values[name]
	if !ok {
		return defaultVal, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid value for --%s: %q", name, v)
	}
	return n, nil
}

// Bool returns true if the flag was provided as a bare --flag.
// Also returns true if the flag was provided with value "true" or "1".
func (f *Flags) Bool(name string) bool {
	if f.bools[name] {
		return true
	}
	if v, ok := f.values[name]; ok {
		return v == "true" || v == "1"
	}
	return false
}

// Duration returns the parsed duration for a flag, or defaultVal if not set.
func (f *Flags) Duration(name string, defaultVal time.Duration) (time.Duration, error) {
	v, ok := f.values[name]
	if !ok {
		return defaultVal, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("invalid duration for --%s: %q", name, v)
	}
	return d, nil
}

// Raw returns the raw string value and whether it was set.
// Useful when the caller needs custom validation beyond what the typed helpers provide.
func (f *Flags) Raw(name string) (string, bool) {
	v, ok := f.values[name]
	return v, ok
}
