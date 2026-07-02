package redaction

import (
	"strings"
	"testing"
)

func TestScanAndRedact_ModeOff(t *testing.T) {
	ResetPatterns()
	input := "my key is sk-ant-abc123def456ghi789jkl012mno345pqr678stu901v"
	result := ScanAndRedact(input, Config{Mode: ModeOff})
	if result.Output != input {
		t.Errorf("ModeOff should return input unchanged, got %q", result.Output)
	}
	if len(result.Findings) != 0 {
		t.Errorf("ModeOff should have no findings, got %d", len(result.Findings))
	}
}

func TestScanAndRedact_ModeWarn(t *testing.T) {
	ResetPatterns()
	input := "my key is sk-ant-abc123def456ghi789jkl012mno345pqr678stu901v"
	result := ScanAndRedact(input, Config{Mode: ModeWarn})
	if result.Output != input {
		t.Errorf("ModeWarn should return input unchanged")
	}
	if len(result.Findings) == 0 {
		t.Error("ModeWarn should detect findings")
	}
	if result.Findings[0].Category != CategoryAnthropicKey {
		t.Errorf("expected ANTHROPIC_KEY, got %s", result.Findings[0].Category)
	}
}

func TestScanAndRedact_ModeRedact(t *testing.T) {
	ResetPatterns()
	key := "sk-ant-abc123def456ghi789jkl012mno345pqr678stu901v"
	input := "my key is " + key
	result := ScanAndRedact(input, Config{Mode: ModeRedact})
	if strings.Contains(result.Output, key) {
		t.Error("ModeRedact should remove the key from output")
	}
	if !strings.Contains(result.Output, "[REDACTED:ANTHROPIC_KEY:") {
		t.Errorf("expected redaction placeholder, got %q", result.Output)
	}
}

func TestScanAndRedact_ModeBlock(t *testing.T) {
	ResetPatterns()
	input := "my key is sk-ant-abc123def456ghi789jkl012mno345pqr678stu901v"
	result := ScanAndRedact(input, Config{Mode: ModeBlock})
	if !result.Blocked {
		t.Error("ModeBlock should set Blocked=true when findings exist")
	}
}

func TestScanAndRedact_NoFindings(t *testing.T) {
	ResetPatterns()
	input := "just a normal string with no secrets"
	result := ScanAndRedact(input, Config{Mode: ModeRedact})
	if result.Output != input {
		t.Error("no-findings input should be returned unchanged")
	}
	if len(result.Findings) != 0 {
		t.Errorf("expected 0 findings, got %d", len(result.Findings))
	}
}

func TestScanAndRedact_GitHubToken(t *testing.T) {
	ResetPatterns()
	token := "ghp_" + strings.Repeat("a", 36)
	input := "token: " + token
	result := ScanAndRedact(input, Config{Mode: ModeRedact})
	if strings.Contains(result.Output, token) {
		t.Error("GitHub token should be redacted")
	}
	if result.Findings[0].Category != CategoryGitHubToken {
		t.Errorf("expected GITHUB_TOKEN, got %s", result.Findings[0].Category)
	}
}

func TestScanAndRedact_JWT(t *testing.T) {
	ResetPatterns()
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.abcdefghijklmnop"
	input := "auth: " + jwt
	result := ScanAndRedact(input, Config{Mode: ModeRedact})
	if strings.Contains(result.Output, jwt) {
		t.Error("JWT should be redacted")
	}
}

func TestScanAndRedact_DatabaseURL(t *testing.T) {
	ResetPatterns()
	url := "postgres://admin:secretpass@db.example.com:5432/mydb"
	input := "dsn=" + url
	result := ScanAndRedact(input, Config{Mode: ModeRedact})
	if strings.Contains(result.Output, "secretpass") {
		t.Error("database URL with credentials should be redacted")
	}
}

func TestScanAndRedact_PrivateKey(t *testing.T) {
	ResetPatterns()
	input := "-----BEGIN RSA PRIVATE KEY-----\nMIIE..."
	result := ScanAndRedact(input, Config{Mode: ModeWarn})
	if len(result.Findings) == 0 {
		t.Error("private key header should be detected")
	}
	if result.Findings[0].Category != CategoryPrivateKey {
		t.Errorf("expected PRIVATE_KEY, got %s", result.Findings[0].Category)
	}
}

func TestScanAndRedact_SlackToken(t *testing.T) {
	ResetPatterns()
	token := "xoxb-123456789-987654321-abcdefghijklmn"
	input := "slack: " + token
	result := ScanAndRedact(input, Config{Mode: ModeRedact})
	if strings.Contains(result.Output, token) {
		t.Error("Slack token should be redacted")
	}
	if result.Findings[0].Category != CategorySlackToken {
		t.Errorf("expected SLACK_TOKEN, got %s", result.Findings[0].Category)
	}
}

func TestScanAndRedact_NotionToken(t *testing.T) {
	ResetPatterns()
	token := "ntn_" + strings.Repeat("x", 45)
	input := "notion: " + token
	result := ScanAndRedact(input, Config{Mode: ModeRedact})
	if strings.Contains(result.Output, token) {
		t.Error("Notion token should be redacted")
	}
}

func TestScanAndRedact_HuggingFaceToken(t *testing.T) {
	ResetPatterns()
	token := "hf_" + strings.Repeat("a", 35)
	input := "hf: " + token
	result := ScanAndRedact(input, Config{Mode: ModeRedact})
	if strings.Contains(result.Output, token) {
		t.Error("HuggingFace token should be redacted")
	}
}

func TestScanAndRedact_Allowlist(t *testing.T) {
	ResetPatterns()
	token := "ghp_" + strings.Repeat("a", 36)
	input := "token: " + token
	cfg := Config{
		Mode:      ModeRedact,
		Allowlist: []string{`ghp_a{36}`},
	}
	result := ScanAndRedact(input, cfg)
	if !strings.Contains(result.Output, token) {
		t.Error("allowlisted token should NOT be redacted")
	}
}

func TestScanAndRedact_DisabledCategory(t *testing.T) {
	ResetPatterns()
	// Use a token that only matches one category (GitHub) and won't match generics.
	// Disable that category and verify it's not detected.
	token := "ghp_" + strings.Repeat("b", 36)
	input := token // no "token:" prefix that would trigger generic patterns
	cfg := Config{
		Mode: ModeWarn,
		DisabledCategories: []Category{
			CategoryGitHubToken,
			CategoryGenericSecret, // "token" in the value could match
			CategoryGenericAPIKey,
			CategoryBearerToken,
		},
	}
	result := ScanAndRedact(input, cfg)
	if len(result.Findings) != 0 {
		t.Errorf("disabled categories should produce no findings, got %d: %v",
			len(result.Findings), result.Findings)
	}
}

func TestScanAndRedact_ExtraPatterns(t *testing.T) {
	ResetPatterns()
	input := "custom_secret=MYAPP-KEY-12345678901234567890"
	cfg := Config{
		Mode: ModeRedact,
		ExtraPatterns: map[Category][]string{
			"CUSTOM_KEY": {`MYAPP-KEY-[a-zA-Z0-9]{20,}`},
		},
	}
	result := ScanAndRedact(input, cfg)
	if strings.Contains(result.Output, "MYAPP-KEY") {
		t.Error("extra pattern should be detected and redacted")
	}
}

func TestScanAndRedact_MultipleFindings(t *testing.T) {
	ResetPatterns()
	key1 := "sk-ant-" + strings.Repeat("a", 43)
	key2 := "ghp_" + strings.Repeat("b", 36)
	input := "keys: " + key1 + " and " + key2
	result := ScanAndRedact(input, Config{Mode: ModeRedact})
	if len(result.Findings) != 2 {
		t.Errorf("expected 2 findings, got %d", len(result.Findings))
	}
	if strings.Contains(result.Output, key1) || strings.Contains(result.Output, key2) {
		t.Error("both keys should be redacted")
	}
}

func TestGeneratePlaceholder_Deterministic(t *testing.T) {
	p1 := generatePlaceholder(CategoryGitHubToken, "ghp_test123")
	p2 := generatePlaceholder(CategoryGitHubToken, "ghp_test123")
	if p1 != p2 {
		t.Error("same input should produce same placeholder")
	}
	p3 := generatePlaceholder(CategoryGitHubToken, "ghp_other456")
	if p1 == p3 {
		t.Error("different input should produce different placeholder")
	}
}

func TestAddLineInfo(t *testing.T) {
	input := "line1\nline2 with secret\nline3"
	findings := []Finding{
		{Start: 12, End: 18}, // "secret" in line 2
	}
	AddLineInfo(input, findings)
	if findings[0].Line != 2 {
		t.Errorf("expected line 2, got %d", findings[0].Line)
	}
	if findings[0].Column != 7 {
		t.Errorf("expected column 7, got %d", findings[0].Column)
	}
}

func TestConvenienceFunctions(t *testing.T) {
	ResetPatterns()
	key := "sk-ant-" + strings.Repeat("c", 43)
	input := "key: " + key

	// Scan
	findings := Scan(input, Config{Mode: ModeRedact})
	if len(findings) == 0 {
		t.Error("Scan should return findings")
	}

	// Redact
	output, findings2 := Redact(input, Config{})
	if strings.Contains(output, key) {
		t.Error("Redact should remove the key")
	}
	if len(findings2) == 0 {
		t.Error("Redact should return findings")
	}

	// ContainsSensitive
	if !ContainsSensitive(input, Config{}) {
		t.Error("ContainsSensitive should return true")
	}
	if ContainsSensitive("just normal text", Config{}) {
		t.Error("ContainsSensitive should return false for clean input")
	}
}

func TestScanAndRedact_TelegramToken(t *testing.T) {
	ResetPatterns()
	token := "12345678:ABCDefghIJKLmnopQRSTuvwxyz123456"
	input := "bot token: " + token
	result := ScanAndRedact(input, Config{Mode: ModeRedact})
	if strings.Contains(result.Output, token) {
		t.Error("Telegram token should be redacted")
	}
	if len(result.Findings) == 0 || result.Findings[0].Category != CategoryTelegramToken {
		cats := ""
		for _, f := range result.Findings {
			cats += string(f.Category) + " "
		}
		t.Errorf("expected TELEGRAM_TOKEN, got: %s", cats)
	}
}

func TestScanAndRedact_PerplexityKey(t *testing.T) {
	ResetPatterns()
	token := "pplx-" + strings.Repeat("a", 48)
	input := "key: " + token
	result := ScanAndRedact(input, Config{Mode: ModeRedact})
	if strings.Contains(result.Output, token) {
		t.Error("Perplexity key should be redacted")
	}
	if result.Findings[0].Category != CategoryPerplexityKey {
		t.Errorf("expected PERPLEXITY_KEY, got %s", result.Findings[0].Category)
	}
}

func TestScanAndRedact_FalKey(t *testing.T) {
	ResetPatterns()
	token := "fal_" + strings.Repeat("x", 32)
	input := "key: " + token
	result := ScanAndRedact(input, Config{Mode: ModeRedact})
	if strings.Contains(result.Output, token) {
		t.Error("Fal key should be redacted")
	}
	if result.Findings[0].Category != CategoryFalKey {
		t.Errorf("expected FAL_KEY, got %s", result.Findings[0].Category)
	}
}

func TestScanAndRedact_FirecrawlKey(t *testing.T) {
	ResetPatterns()
	token := "fc-" + strings.Repeat("a", 32)
	input := "key: " + token
	result := ScanAndRedact(input, Config{Mode: ModeRedact})
	if strings.Contains(result.Output, token) {
		t.Error("Firecrawl key should be redacted")
	}
	if result.Findings[0].Category != CategoryFirecrawlKey {
		t.Errorf("expected FIRECRAWL_KEY, got %s", result.Findings[0].Category)
	}
}

func TestScanAndRedact_BrowserBaseKey(t *testing.T) {
	ResetPatterns()
	token := "bb_live_" + strings.Repeat("z", 32)
	input := "key: " + token
	result := ScanAndRedact(input, Config{Mode: ModeRedact})
	if strings.Contains(result.Output, token) {
		t.Error("BrowserBase key should be redacted")
	}
	if result.Findings[0].Category != CategoryBrowserBaseKey {
		t.Errorf("expected BROWSERBASE_KEY, got %s", result.Findings[0].Category)
	}
}

func TestScanAndRedact_CodexToken(t *testing.T) {
	ResetPatterns()
	token := "gAAAA" + strings.Repeat("B", 30)
	input := "token: " + token
	result := ScanAndRedact(input, Config{Mode: ModeRedact})
	if strings.Contains(result.Output, token) {
		t.Error("Codex token should be redacted")
	}
	if result.Findings[0].Category != CategoryCodexToken {
		t.Errorf("expected CODEX_TOKEN, got %s", result.Findings[0].Category)
	}
}

func TestConfigValidate(t *testing.T) {
	good := Config{Mode: ModeRedact}
	if err := good.Validate(); err != nil {
		t.Errorf("valid config should not error: %v", err)
	}

	bad := Config{Mode: "invalid"}
	if err := bad.Validate(); err == nil {
		t.Error("invalid mode should error")
	}
}
