package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mistakeknot/intercore/internal/publish"
)

func TestPublishScopedFlagReachesEngine(t *testing.T) {
	root := t.TempDir()
	pluginRoot := filepath.Join(root, "demo")
	for path, body := range map[string]string{
		filepath.Join(pluginRoot, ".claude-plugin", "plugin.json"):                       `{"name":"demo","version":"1.0.0"}`,
		filepath.Join(root, "core", "marketplace", ".claude-plugin", "marketplace.json"): `{"name":"interagency-marketplace","plugins":[{"name":"demo","version":"1.0.0"}]}`,
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	priorDB := flagDB
	flagDB = filepath.Join(root, "uninitialized.db")
	t.Cleanup(func() { flagDB = priorDB })
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	priorStdout := os.Stdout
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = priorStdout })
	code := cmdPublish(context.Background(), []string{"--patch", "--scoped", "--dry-run", "--cwd=" + pluginRoot})
	w.Close()
	os.Stdout = priorStdout
	output, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 || !strings.Contains(string(output), "Scoped publish") {
		t.Fatalf("--scoped not propagated: exit %d, %s", code, output)
	}
}

func TestPublishDoctorExitCode(t *testing.T) {
	tests := []struct {
		name     string
		findings []publish.Finding
		want     int
	}{
		{name: "healthy", want: 0},
		{name: "warning only", findings: []publish.Finding{{Severity: "warning"}}, want: 0},
		{name: "error", findings: []publish.Finding{{Severity: "error"}}, want: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := publishDoctorExitCode(tt.findings); got != tt.want {
				t.Fatalf("publishDoctorExitCode() = %d, want %d", got, tt.want)
			}
		})
	}
}
