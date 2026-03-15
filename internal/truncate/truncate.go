// Package truncate provides output truncation utilities for command results.
package truncate

import "fmt"

// HeadTail truncates s to maxLen using a 40/60 head/tail split.
// The first 40% is kept (error messages appear early) and the last 60%
// (most recent/relevant output). A notice is inserted between the two halves.
// If s is within maxLen, it is returned unchanged.
func HeadTail(s string, maxLen int) string {
	if len(s) <= maxLen || maxLen <= 0 {
		return s
	}

	headLen := maxLen * 2 / 5 // 40%
	tailLen := maxLen - headLen // 60%

	// Ensure we leave room for the notice
	omitted := len(s) - headLen - tailLen
	notice := fmt.Sprintf(
		"\n\n... [OUTPUT TRUNCATED — %d chars omitted out of %d total] ...\n\n",
		omitted, len(s),
	)

	// If the notice itself would consume all our budget, just do a simple tail cut
	if headLen+tailLen+len(notice) > len(s) {
		return s
	}

	return s[:headLen] + notice + s[len(s)-tailLen:]
}

// HeadTailLines truncates s to approximately maxLines using a 40/60 split
// by line count. Operates on newline-delimited text.
func HeadTailLines(s string, maxLines int) string {
	if maxLines <= 0 {
		return s
	}

	lines := splitLines(s)
	if len(lines) <= maxLines {
		return s
	}

	headLines := maxLines * 2 / 5
	tailLines := maxLines - headLines
	omitted := len(lines) - headLines - tailLines

	notice := fmt.Sprintf(
		"\n... [%d lines omitted out of %d total] ...\n",
		omitted, len(lines),
	)

	result := joinLines(lines[:headLines]) + notice + joinLines(lines[len(lines)-tailLines:])
	return result
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i+1])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

func joinLines(lines []string) string {
	var out string
	for _, l := range lines {
		out += l
	}
	return out
}
