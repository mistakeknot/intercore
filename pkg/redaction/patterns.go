package redaction

import (
	"regexp"
	"sync"
)

// Split string literals to avoid triggering secret scanners on the patterns themselves.
const (
	openAIPrefixPattern     = "s" + "k\\-"
	openAIProjPrefixPattern = openAIPrefixPattern + "proj\\-"
	openAIMarker            = "T3Blbk" + "FJ"
)

// patternDef defines a pattern with its category and priority.
type patternDef struct {
	category Category
	pattern  string
	priority int // higher = more specific, takes precedence
}

// pattern represents a compiled detection pattern.
type pattern struct {
	category Category
	regex    *regexp.Regexp
	priority int
}

// defaultPatterns contains all built-in detection patterns.
// Higher priority patterns are checked first and take precedence.
var defaultPatterns = []patternDef{
	// Provider-specific API keys (high priority)
	{CategoryOpenAIKey, openAIPrefixPattern + `[a-zA-Z0-9]{10,}` + openAIMarker + `[a-zA-Z0-9]{10,}`, 100},
	{CategoryOpenAIKey, openAIProjPrefixPattern + `[a-zA-Z0-9_-]{40,}`, 100},
	{CategoryOpenAIKey, openAIPrefixPattern + `[a-zA-Z0-9]{48}`, 95},
	{CategoryAnthropicKey, `sk\-ant\-[a-zA-Z0-9_-]{40,}`, 100},
	{CategoryGitHubToken, `gh[pousr]_[a-zA-Z0-9]{30,}`, 100},
	{CategoryGitHubToken, `github_pat_[a-zA-Z0-9]{20,}_[a-zA-Z0-9]{40,}`, 100},
	{CategoryGoogleAPIKey, `AIza[a-zA-Z0-9_-]{35}`, 100},

	// Cloud provider credentials
	{CategoryAWSAccessKey, `AKIA[0-9A-Z]{16}`, 90},
	{CategoryAWSAccessKey, `ASIA[0-9A-Z]{16}`, 90},
	{CategoryAWSSecretKey, `(?i)(aws_secret|secret_access_key|secret_key)\s*[=:]\s*["']?[a-zA-Z0-9/+=]{40}["']?`, 90},

	// Authentication tokens
	{CategoryJWT, `eyJ[a-zA-Z0-9_-]*\.eyJ[a-zA-Z0-9_-]*\.[a-zA-Z0-9_-]+`, 85},
	{CategoryBearerToken, `(?i)bearer\s+[a-zA-Z0-9._-]{20,}`, 80},

	// Private keys
	{CategoryPrivateKey, `-----BEGIN\s+(RSA\s+|DSA\s+|EC\s+|OPENSSH\s+)?PRIVATE KEY-----`, 95},

	// Database URLs with credentials
	{CategoryDatabaseURL, `(?i)(postgres|mysql|mongodb|redis)://[^:]+:[^@]+@[^\s]+`, 85},

	// Demarch ecosystem tokens
	{CategoryNotionToken, `ntn_[a-zA-Z0-9]{40,}`, 100},
	{CategoryNotionToken, `secret_[a-zA-Z0-9]{40,}`, 80},
	{CategorySlackToken, `xoxb-[0-9]+-[0-9]+-[a-zA-Z0-9]+`, 100},
	{CategorySlackToken, `xoxp-[0-9]+-[0-9]+-[0-9]+-[a-f0-9]+`, 100},
	{CategorySlackToken, `xoxe\.xox[bp]-[0-9]-[a-zA-Z0-9]+`, 100},
	{CategoryHuggingFace, `hf_[a-zA-Z0-9]{30,}`, 100},
	{CategoryExaAPIKey, `(?i)(exa[_-]?api[_-]?key)\s*[=:]\s*["']?[a-zA-Z0-9_-]{20,}["']?`, 80},

	// Generic patterns (lower priority)
	{CategoryPassword, `(?i)(password|passwd|pwd)\s*[=:]\s*["']?[^\s"']{8,}["']?`, 50},
	{CategoryGenericAPIKey, `(?i)([a-z_]*api[_]?key)\s*[=:]\s*["']?[a-zA-Z0-9_-]{16,}["']?`, 40},
	{CategoryGenericSecret, `(?i)(secret|private[_]?key|token)\s*[=:]\s*["']?[a-zA-Z0-9/+=_-]{16,}["']?`, 30},
}

var compiledPatterns []pattern
var compileOnce sync.Once

// ResetPatterns resets compiled patterns (for testing only).
func ResetPatterns() {
	compileOnce = sync.Once{}
	compiledPatterns = nil
}

func compilePatterns() {
	compileOnce.Do(func() {
		compiledPatterns = make([]pattern, 0, len(defaultPatterns))
		for _, def := range defaultPatterns {
			re, err := regexp.Compile(def.pattern)
			if err != nil {
				continue
			}
			compiledPatterns = append(compiledPatterns, pattern{
				category: def.category,
				regex:    re,
				priority: def.priority,
			})
		}
		sortPatternsByPriority(compiledPatterns)
	})
}

func sortPatternsByPriority(patterns []pattern) {
	for i := 1; i < len(patterns); i++ {
		j := i
		for j > 0 && patterns[j].priority > patterns[j-1].priority {
			patterns[j], patterns[j-1] = patterns[j-1], patterns[j]
			j--
		}
	}
}

func getPatterns() []pattern {
	compilePatterns()
	return compiledPatterns
}

// getExtraPatterns compiles and returns extra patterns from config.
func getExtraPatterns(extra map[Category][]string) []pattern {
	if len(extra) == 0 {
		return nil
	}
	var patterns []pattern
	for cat, pats := range extra {
		for _, p := range pats {
			re, err := regexp.Compile(p)
			if err != nil {
				continue
			}
			patterns = append(patterns, pattern{
				category: cat,
				regex:    re,
				priority: 60, // between provider-specific and generic
			})
		}
	}
	return patterns
}

func compileAllowlist(allowlist []string) []*regexp.Regexp {
	if len(allowlist) == 0 {
		return nil
	}
	compiled := make([]*regexp.Regexp, 0, len(allowlist))
	for _, pat := range allowlist {
		re, err := regexp.Compile(pat)
		if err != nil {
			continue
		}
		compiled = append(compiled, re)
	}
	return compiled
}

func isAllowlisted(match string, allowlist []*regexp.Regexp) bool {
	for _, re := range allowlist {
		if re.MatchString(match) {
			return true
		}
	}
	return false
}

func isCategoryDisabled(cat Category, disabled []Category) bool {
	for _, d := range disabled {
		if d == cat {
			return true
		}
	}
	return false
}
