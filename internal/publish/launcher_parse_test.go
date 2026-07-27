package publish

import (
	"os"
	"path/filepath"
	"testing"
)

func writeLauncher(t *testing.T, root, body string) string {
	t.Helper()
	dir := filepath.Join(root, "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "launch-mcp.sh")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParseLauncherScriptForms(t *testing.T) {
	cases := []struct {
		name       string
		line       string
		wantTarget string
		wantBinary string
	}{
		{
			// The form every marketplace launcher actually uses. The previous
			// pattern required a "/" inside the -o argument and matched none
			// of them, so the pre-build silently did nothing everywhere.
			name:       "variable output with trailing slash on target",
			line:       `go build -o "$BINARY" ./cmd/interlock-mcp/ 2>&1 >&2`,
			wantTarget: "./cmd/interlock-mcp",
			wantBinary: "interlock-mcp",
		},
		{
			name:       "literal output path",
			line:       `go build -o "$SCRIPT_DIR/server" ./cmd/server`,
			wantTarget: "./cmd/server",
			wantBinary: "server",
		},
		{
			name:       "braced variable output",
			line:       `go build -o "${BINARY}" ./cmd/thing`,
			wantTarget: "./cmd/thing",
			wantBinary: "thing",
		},
		{
			name:       "flags between build and -o",
			line:       `go build -trimpath -o "$BINARY" ./cmd/thing`,
			wantTarget: "./cmd/thing",
			wantBinary: "thing",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeLauncher(t, t.TempDir(), "#!/usr/bin/env bash\nset -e\n"+tc.line+"\n")
			target, binary := parseLauncherScript(path)
			if target != tc.wantTarget || binary != tc.wantBinary {
				t.Fatalf("parse = (%q, %q), want (%q, %q)", target, binary, tc.wantTarget, tc.wantBinary)
			}
		})
	}
}

// TestParseLauncherScriptIgnoresComments: launchers describe their fallback
// behaviour in prose, and "falling back to go build" must not be read as a
// build instruction.
func TestParseLauncherScriptIgnoresComments(t *testing.T) {
	body := "#!/usr/bin/env bash\n" +
		"# Probes known paths before falling back to go build -o whatever ./cmd/wrong\n" +
		`go build -o "$BINARY" ./cmd/right` + "\n"
	target, binary := parseLauncherScript(writeLauncher(t, t.TempDir(), body))
	if target != "./cmd/right" || binary != "right" {
		t.Fatalf("parse = (%q, %q), want the real build line", target, binary)
	}
}

// TestParseLauncherScriptNoBuildLine: an unparseable launcher must report
// nothing found so the caller can complain, rather than silently succeeding.
func TestParseLauncherScriptNoBuildLine(t *testing.T) {
	body := "#!/usr/bin/env bash\nexec \"$SCRIPT_DIR/prebuilt\" \"$@\"\n"
	target, binary := parseLauncherScript(writeLauncher(t, t.TempDir(), body))
	if target != "" || binary != "" {
		t.Fatalf("parse = (%q, %q), want empty", target, binary)
	}
}

// TestBuildGoMCPBinaryReportsUnparseableLauncher: a plugin with go.mod AND a
// launcher that yields no target is a defect, not a non-event. Returning nil
// here is what let the broken parser go unnoticed.
func TestBuildGoMCPBinaryReportsUnparseableLauncher(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module demo\n\ngo 1.25.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeLauncher(t, root, "#!/usr/bin/env bash\nexec \"$SCRIPT_DIR/prebuilt\" \"$@\"\n")

	err := BuildGoMCPBinary("demo", root, t.TempDir())
	if err == nil {
		t.Fatal("unparseable launcher returned nil; the skip is invisible again")
	}
}

// TestBuildGoMCPBinarySilentOnNonGoPlugin: the two genuine non-events stay
// quiet. Warning about every non-Go plugin on every publish would train the
// operator to ignore the channel the case above needs.
func TestBuildGoMCPBinarySilentOnNonGoPlugin(t *testing.T) {
	if err := BuildGoMCPBinary("demo", t.TempDir(), t.TempDir()); err != nil {
		t.Fatalf("plugin without go.mod = %v, want nil", err)
	}

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module demo\n\ngo 1.25.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := BuildGoMCPBinary("demo", root, t.TempDir()); err != nil {
		t.Fatalf("Go plugin without a launcher = %v, want nil", err)
	}
}

// TestShippedLaunchersParse pins the parser against the launchers actually in
// the marketplace. A unit test over synthetic strings would have passed for the
// whole time the real ones were unparseable.
func TestShippedLaunchersParse(t *testing.T) {
	for _, plugin := range []string{"interlab", "interlock", "intermap", "intermix", "intermux"} {
		path := filepath.Join("..", "..", "..", "..", "interverse", plugin, "bin", "launch-mcp.sh")
		if _, err := os.Stat(path); err != nil {
			t.Logf("%s: not present beside intercore, skipping", plugin)
			continue
		}
		target, binary := parseLauncherScript(path)
		if target == "" || binary == "" {
			t.Errorf("%s: launcher does not parse (target=%q binary=%q)", plugin, target, binary)
			continue
		}
		if want := plugin + "-mcp"; binary != want {
			t.Errorf("%s: binary name = %q, want %q", plugin, binary, want)
		}
	}
}
