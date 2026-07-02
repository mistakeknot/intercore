// Package redaction provides detection and redaction of sensitive content
// such as API keys, tokens, passwords, and other secrets.
//
// Ported from github.com/Dicklesworthstone/ntm/internal/redaction with
// additional categories for the Demarch ecosystem.
//
// Exported via pkg/ so both intercore and autarch can import it.
package redaction

// Mode defines the redaction behavior.
type Mode string

const (
	// ModeOff disables all scanning and redaction.
	ModeOff Mode = "off"
	// ModeWarn scans and logs findings but doesn't modify content.
	ModeWarn Mode = "warn"
	// ModeRedact replaces sensitive content with placeholders.
	ModeRedact Mode = "redact"
	// ModeBlock fails the operation if sensitive content is detected.
	ModeBlock Mode = "block"
)

// Category identifies the type of sensitive content detected.
type Category string

const (
	// Provider-specific API keys
	CategoryOpenAIKey    Category = "OPENAI_KEY"
	CategoryAnthropicKey Category = "ANTHROPIC_KEY"
	CategoryGitHubToken  Category = "GITHUB_TOKEN"
	CategoryGoogleAPIKey Category = "GOOGLE_API_KEY"

	// Cloud provider credentials
	CategoryAWSAccessKey Category = "AWS_ACCESS_KEY"
	CategoryAWSSecretKey Category = "AWS_SECRET_KEY"

	// Authentication tokens
	CategoryJWT         Category = "JWT"
	CategoryBearerToken Category = "BEARER_TOKEN"
	CategoryPrivateKey  Category = "PRIVATE_KEY"

	// Connection strings
	CategoryDatabaseURL Category = "DATABASE_URL"

	// Generic patterns
	CategoryPassword      Category = "PASSWORD"
	CategoryGenericAPIKey Category = "GENERIC_API_KEY"
	CategoryGenericSecret Category = "GENERIC_SECRET"

	// Demarch ecosystem
	CategoryNotionToken Category = "NOTION_TOKEN"
	CategorySlackToken  Category = "SLACK_TOKEN"
	CategoryHuggingFace Category = "HUGGINGFACE_TOKEN"
	CategoryExaAPIKey   Category = "EXA_API_KEY"

	// Hermes-discovered patterns
	CategoryTelegramToken  Category = "TELEGRAM_TOKEN"
	CategoryPerplexityKey  Category = "PERPLEXITY_KEY"
	CategoryFalKey         Category = "FAL_KEY"
	CategoryFirecrawlKey   Category = "FIRECRAWL_KEY"
	CategoryBrowserBaseKey Category = "BROWSERBASE_KEY"
	CategoryCodexToken     Category = "CODEX_TOKEN"
)

// Finding represents a single detected secret.
type Finding struct {
	Category Category `json:"category"`
	Match    string   `json:"match"`
	Redacted string   `json:"redacted"`
	Start    int      `json:"start"`
	End      int      `json:"end"`
	Line     int      `json:"line,omitempty"`
	Column   int      `json:"column,omitempty"`
}

// Result contains the outcome of a scan/redaction operation.
type Result struct {
	Mode           Mode      `json:"mode"`
	Findings       []Finding `json:"findings"`
	Output         string    `json:"output"`
	Blocked        bool      `json:"blocked"`
	OriginalLength int       `json:"original_length"`
}

// Config configures the redaction behavior.
type Config struct {
	Mode               Mode                  `json:"mode"`
	Allowlist          []string              `json:"allowlist,omitempty"`
	ExtraPatterns      map[Category][]string `json:"extra_patterns,omitempty"`
	DisabledCategories []Category            `json:"disabled_categories,omitempty"`
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() Config {
	return Config{
		Mode: ModeWarn,
	}
}

// Validate checks if the config is valid.
func (c *Config) Validate() error {
	switch c.Mode {
	case ModeOff, ModeWarn, ModeRedact, ModeBlock:
		// valid
	default:
		return &ConfigError{Field: "mode", Message: "invalid mode: " + string(c.Mode)}
	}
	return nil
}

// ConfigError represents a configuration error.
type ConfigError struct {
	Field   string
	Message string
}

func (e *ConfigError) Error() string {
	return "redaction config error: " + e.Field + ": " + e.Message
}
