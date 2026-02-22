package redaction

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
)

// ScanAndRedact scans input for sensitive content and optionally redacts it.
// The behavior depends on the mode in cfg:
//   - ModeOff: returns input unchanged with no findings
//   - ModeWarn: scans and reports findings but doesn't modify output
//   - ModeRedact: replaces sensitive content with placeholders
//   - ModeBlock: scans and sets Blocked=true if findings exist
func ScanAndRedact(input string, cfg Config) Result {
	result := Result{
		Mode:           cfg.Mode,
		OriginalLength: len(input),
	}

	if cfg.Mode == ModeOff {
		result.Output = input
		return result
	}

	allowlist := compileAllowlist(cfg.Allowlist)
	matches := scan(input, allowlist, cfg.DisabledCategories, cfg.ExtraPatterns)

	if len(matches) == 0 {
		result.Output = input
		return result
	}

	result.Findings = make([]Finding, len(matches))
	for i, m := range matches {
		result.Findings[i] = Finding{
			Category: m.category,
			Match:    m.match,
			Redacted: generatePlaceholder(m.category, m.match),
			Start:    m.start,
			End:      m.end,
		}
	}

	switch cfg.Mode {
	case ModeWarn:
		result.Output = input
	case ModeRedact:
		result.Output = applyRedactions(input, result.Findings)
	case ModeBlock:
		result.Output = input
		result.Blocked = true
	}

	return result
}

type match struct {
	category Category
	match    string
	start    int
	end      int
	priority int
}

func scan(input string, allowlist []*regexp.Regexp, disabled []Category, extra map[Category][]string) []match {
	patterns := getPatterns()

	// Append any extra patterns from config.
	if ep := getExtraPatterns(extra); len(ep) > 0 {
		patterns = append(patterns, ep...)
	}

	var allMatches []match

	for _, p := range patterns {
		if isCategoryDisabled(p.category, disabled) {
			continue
		}

		locs := p.regex.FindAllStringIndex(input, -1)
		for _, loc := range locs {
			allMatches = append(allMatches, match{
				category: p.category,
				match:    input[loc[0]:loc[1]],
				start:    loc[0],
				end:      loc[1],
				priority: p.priority,
			})
		}
	}

	deduplicated := deduplicateMatches(allMatches)

	if len(allowlist) > 0 {
		var filtered []match
		for _, m := range deduplicated {
			if !isAllowlisted(m.match, allowlist) {
				filtered = append(filtered, m)
			}
		}
		return filtered
	}

	return deduplicated
}

func deduplicateMatches(matches []match) []match {
	if len(matches) == 0 {
		return matches
	}

	sort.Slice(matches, func(i, j int) bool {
		if matches[i].priority != matches[j].priority {
			return matches[i].priority > matches[j].priority
		}
		return matches[i].start < matches[j].start
	})

	maxEnd := 0
	for _, m := range matches {
		if m.end > maxEnd {
			maxEnd = m.end
		}
	}

	covered := make([]bool, maxEnd+1)
	var result []match

	for _, m := range matches {
		overlaps := false
		for i := m.start; i < m.end; i++ {
			if covered[i] {
				overlaps = true
				break
			}
		}

		if !overlaps {
			result = append(result, m)
			for i := m.start; i < m.end; i++ {
				covered[i] = true
			}
		}
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].start < result[j].start
	})

	return result
}

// generatePlaceholder creates a deterministic redaction placeholder.
// Format: [REDACTED:CATEGORY:hash8]
func generatePlaceholder(cat Category, content string) string {
	data := string(cat) + ":" + content
	hash := sha256.Sum256([]byte(data))
	hashStr := hex.EncodeToString(hash[:4])
	return fmt.Sprintf("[REDACTED:%s:%s]", cat, hashStr)
}

func applyRedactions(input string, findings []Finding) string {
	if len(findings) == 0 {
		return input
	}

	sorted := make([]Finding, len(findings))
	copy(sorted, findings)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Start > sorted[j].Start
	})

	result := input
	for _, f := range sorted {
		if f.Start >= 0 && f.End <= len(result) && f.Start < f.End {
			result = result[:f.Start] + f.Redacted + result[f.End:]
		}
	}

	return result
}

// Scan performs read-only detection without redaction.
func Scan(input string, cfg Config) []Finding {
	cfg.Mode = ModeWarn
	result := ScanAndRedact(input, cfg)
	return result.Findings
}

// Redact is a convenience function that performs redaction.
func Redact(input string, cfg Config) (string, []Finding) {
	cfg.Mode = ModeRedact
	result := ScanAndRedact(input, cfg)
	return result.Output, result.Findings
}

// ContainsSensitive checks if input contains any sensitive content.
func ContainsSensitive(input string, cfg Config) bool {
	cfg.Mode = ModeWarn
	result := ScanAndRedact(input, cfg)
	return len(result.Findings) > 0
}

// AddLineInfo enriches findings with line and column information.
func AddLineInfo(input string, findings []Finding) {
	if len(findings) == 0 {
		return
	}

	lineStarts := []int{0}
	for i, c := range input {
		if c == '\n' {
			lineStarts = append(lineStarts, i+1)
		}
	}

	for i := range findings {
		pos := findings[i].Start
		line := sort.Search(len(lineStarts), func(j int) bool {
			return lineStarts[j] > pos
		}) - 1
		if line < 0 {
			line = 0
		}
		findings[i].Line = line + 1
		findings[i].Column = pos - lineStarts[line] + 1
	}
}
