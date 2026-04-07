package cli

import (
	"testing"
	"time"
)

func TestParseFlags_EqualsSyntax(t *testing.T) {
	f := ParseFlags([]string{"--name=foo", "--count=42"})

	if got := f.String("name", ""); got != "foo" {
		t.Errorf("String(name) = %q, want %q", got, "foo")
	}
	n, err := f.Int("count", 0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 42 {
		t.Errorf("Int(count) = %d, want 42", n)
	}
	if len(f.Positionals) != 0 {
		t.Errorf("Positionals = %v, want empty", f.Positionals)
	}
}

func TestParseFlags_BareDoubleDashIsBool(t *testing.T) {
	// --name without = is a boolean, "foo" becomes positional
	f := ParseFlags([]string{"--name", "foo"})

	if !f.Bool("name") {
		t.Error("Bool(name) = false, want true")
	}
	if len(f.Positionals) != 1 || f.Positionals[0] != "foo" {
		t.Errorf("Positionals = %v, want [foo]", f.Positionals)
	}
}

func TestParseFlags_BoolFlags(t *testing.T) {
	f := ParseFlags([]string{"--verbose", "--follow"})

	if !f.Bool("verbose") {
		t.Error("Bool(verbose) = false, want true")
	}
	if !f.Bool("follow") {
		t.Error("Bool(follow) = false, want true")
	}
	if f.Bool("missing") {
		t.Error("Bool(missing) = true, want false")
	}
}

func TestParseFlags_BoolBetweenFlags(t *testing.T) {
	// --active is a bool because the next arg is also a --flag
	f := ParseFlags([]string{"--active", "--scope=test"})

	if !f.Bool("active") {
		t.Error("Bool(active) = false, want true")
	}
	if got := f.String("scope", ""); got != "test" {
		t.Errorf("String(scope) = %q, want %q", got, "test")
	}
}

func TestParseFlags_Positionals(t *testing.T) {
	f := ParseFlags([]string{"myid", "--name=foo", "extra"})

	if len(f.Positionals) != 2 {
		t.Fatalf("Positionals = %v, want [myid extra]", f.Positionals)
	}
	if f.Positionals[0] != "myid" || f.Positionals[1] != "extra" {
		t.Errorf("Positionals = %v, want [myid extra]", f.Positionals)
	}
	if got := f.String("name", ""); got != "foo" {
		t.Errorf("String(name) = %q, want %q", got, "foo")
	}
}

func TestParseFlags_ShortFlagsArePositional(t *testing.T) {
	f := ParseFlags([]string{"-f", "--name=foo"})

	if len(f.Positionals) != 1 || f.Positionals[0] != "-f" {
		t.Errorf("Positionals = %v, want [-f]", f.Positionals)
	}
}

func TestParseFlags_Duration(t *testing.T) {
	f := ParseFlags([]string{"--timeout=30s", "--poll=500ms"})

	d, err := f.Duration("timeout", 0)
	if err != nil {
		t.Fatal(err)
	}
	if d != 30*time.Second {
		t.Errorf("Duration(timeout) = %v, want 30s", d)
	}

	d, err = f.Duration("poll", 0)
	if err != nil {
		t.Fatal(err)
	}
	if d != 500*time.Millisecond {
		t.Errorf("Duration(poll) = %v, want 500ms", d)
	}
}

func TestParseFlags_DurationDefault(t *testing.T) {
	f := ParseFlags([]string{})

	d, err := f.Duration("timeout", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if d != 5*time.Second {
		t.Errorf("Duration(timeout) = %v, want 5s", d)
	}
}

func TestParseFlags_IntDefault(t *testing.T) {
	f := ParseFlags([]string{})

	n, err := f.Int("count", 99)
	if err != nil {
		t.Fatal(err)
	}
	if n != 99 {
		t.Errorf("Int(count) = %d, want 99", n)
	}
}

func TestParseFlags_IntError(t *testing.T) {
	f := ParseFlags([]string{"--count=abc"})

	_, err := f.Int("count", 0)
	if err == nil {
		t.Error("Int(count=abc) should return error")
	}
}

func TestParseFlags_Int64(t *testing.T) {
	f := ParseFlags([]string{"--offset=1234567890"})

	n, err := f.Int64("offset", 0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1234567890 {
		t.Errorf("Int64(offset) = %d, want 1234567890", n)
	}
}

func TestParseFlags_StringPtr(t *testing.T) {
	f := ParseFlags([]string{"--scope=test"})

	if p := f.StringPtr("scope"); p == nil {
		t.Error("StringPtr(scope) = nil, want non-nil")
	} else if *p != "test" {
		t.Errorf("StringPtr(scope) = %q, want %q", *p, "test")
	}

	if p := f.StringPtr("missing"); p != nil {
		t.Errorf("StringPtr(missing) = %q, want nil", *p)
	}
}

func TestParseFlags_Has(t *testing.T) {
	f := ParseFlags([]string{"--name=foo", "--verbose"})

	if !f.Has("name") {
		t.Error("Has(name) = false, want true")
	}
	if !f.Has("verbose") {
		t.Error("Has(verbose) = false, want true")
	}
	if f.Has("missing") {
		t.Error("Has(missing) = true, want false")
	}
}

func TestParseFlags_Raw(t *testing.T) {
	f := ParseFlags([]string{"--name=foo"})

	v, ok := f.Raw("name")
	if !ok || v != "foo" {
		t.Errorf("Raw(name) = (%q, %v), want (%q, true)", v, ok, "foo")
	}

	_, ok = f.Raw("missing")
	if ok {
		t.Error("Raw(missing) ok = true, want false")
	}
}

func TestParseFlags_EmptyValue(t *testing.T) {
	f := ParseFlags([]string{"--name="})

	if got := f.String("name", "default"); got != "" {
		t.Errorf("String(name) = %q, want empty string", got)
	}
}

func TestParseFlags_Mixed(t *testing.T) {
	// Realistic dispatch spawn args
	f := ParseFlags([]string{
		"--type=codex",
		"--prompt-file=/tmp/prompt.md",
		"--project=/home/user/proj",
		"--scheduled",
		"--timeout=5m",
		"--scope-id=run-123",
	})

	if got := f.String("type", ""); got != "codex" {
		t.Errorf("type = %q, want codex", got)
	}
	if got := f.String("prompt-file", ""); got != "/tmp/prompt.md" {
		t.Errorf("prompt-file = %q", got)
	}
	if !f.Bool("scheduled") {
		t.Error("scheduled = false")
	}
	d, err := f.Duration("timeout", 0)
	if err != nil {
		t.Fatal(err)
	}
	if d != 5*time.Minute {
		t.Errorf("timeout = %v, want 5m", d)
	}
}

func TestParseFlags_BoolValueTrue(t *testing.T) {
	f := ParseFlags([]string{"--verbose=true"})
	if !f.Bool("verbose") {
		t.Error("Bool(verbose=true) = false, want true")
	}
}

func TestParseFlags_NoArgs(t *testing.T) {
	f := ParseFlags([]string{})
	if len(f.Positionals) != 0 {
		t.Errorf("Positionals = %v, want empty", f.Positionals)
	}
	if f.String("anything", "def") != "def" {
		t.Error("default not returned for empty flags")
	}
}
