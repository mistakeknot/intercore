package publish

import (
	"fmt"
	"regexp"
	"strconv"
)

var semverRe = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)(-[a-zA-Z0-9.]+)?$`)

// ParseVersion parses a semver string into its components.
func ParseVersion(s string) (major, minor, patch int, pre string, err error) {
	m := semverRe.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, 0, "", fmt.Errorf("invalid semver: %q", s)
	}
	major, _ = strconv.Atoi(m[1])
	minor, _ = strconv.Atoi(m[2])
	patch, _ = strconv.Atoi(m[3])
	if m[4] != "" {
		pre = m[4][1:] // strip leading '-'
	}
	return major, minor, patch, pre, nil
}

// FormatVersion formats a semver from its components.
func FormatVersion(major, minor, patch int, pre string) string {
	v := fmt.Sprintf("%d.%d.%d", major, minor, patch)
	if pre != "" {
		v += "-" + pre
	}
	return v
}

// BumpVersion increments a version string according to the given mode.
func BumpVersion(current string, mode BumpMode) (string, error) {
	major, minor, patch, _, err := ParseVersion(current)
	if err != nil {
		return "", err
	}
	switch mode {
	case BumpPatch:
		return FormatVersion(major, minor, patch+1, ""), nil
	case BumpMinor:
		return FormatVersion(major, minor+1, 0, ""), nil
	default:
		return "", fmt.Errorf("BumpVersion: unsupported mode %d (use BumpPatch or BumpMinor)", mode)
	}
}

// CompareVersions returns -1 if a < b, 0 if equal, 1 if a > b.
// Pre-release versions sort before their release counterpart.
func CompareVersions(a, b string) int {
	aMaj, aMin, aPat, aPre, aErr := ParseVersion(a)
	bMaj, bMin, bPat, bPre, bErr := ParseVersion(b)
	if aErr != nil || bErr != nil {
		// Fall back to string comparison for unparseable versions.
		if a < b {
			return -1
		}
		if a > b {
			return 1
		}
		return 0
	}

	if aMaj != bMaj {
		return cmpInt(aMaj, bMaj)
	}
	if aMin != bMin {
		return cmpInt(aMin, bMin)
	}
	if aPat != bPat {
		return cmpInt(aPat, bPat)
	}

	// Pre-release < release (e.g., 1.0.0-alpha < 1.0.0)
	if aPre != "" && bPre == "" {
		return -1
	}
	if aPre == "" && bPre != "" {
		return 1
	}
	// Both have pre-release: lexicographic
	if aPre < bPre {
		return -1
	}
	if aPre > bPre {
		return 1
	}
	return 0
}

func cmpInt(a, b int) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}
