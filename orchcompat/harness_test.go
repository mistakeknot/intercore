package orchcompat

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/mistakeknot/intercore/internal/testutil"
	"gopkg.in/yaml.v3"
)

var update = flag.Bool("update", false, "rewrite golden files from the current ic behavior")

var icBinary string

func TestMain(m *testing.M) {
	os.Unsetenv("IC_BACKUP_DIR")
	os.Unsetenv("IC_BACKUP_KEEP")
	flag.Parse()
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	if prebuilt := os.Getenv("ORCHCOMPAT_IC"); prebuilt != "" {
		icBinary = prebuilt
		return m.Run()
	}
	dir, err := os.MkdirTemp("", "orchcompat-ic-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(dir)
	icBinary = filepath.Join(dir, "ic")
	buildEnv, err := hermeticBuildEnv(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	build := exec.Command("go", "build", "-buildvcs=false", "-o", icBinary, "./cmd/ic")
	build.Dir = ".."
	build.Env = buildEnv
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "building ic: %v\n%s", err, out)
		return 1
	}
	return m.Run()
}

func hermeticBuildEnv(dir string) ([]string, error) {
	query := func(name string) (string, error) {
		if v := os.Getenv(name); v != "" {
			return v, nil
		}
		cmd := exec.Command("go", "env", name)
		cmd.Env = append(os.Environ(),
			"GOTOOLCHAIN=local", "GOTELEMETRY=off", "GOPROXY=off", "GOFLAGS=", "GOWORK=off")
		out, err := cmd.Output()
		if err != nil {
			return "", fmt.Errorf("go env %s: %w", name, err)
		}
		return strings.TrimSpace(string(out)), nil
	}
	goroot, err := query("GOROOT")
	if err != nil {
		return nil, err
	}
	modcache, err := query("GOMODCACHE")
	if err != nil {
		return nil, err
	}
	gocache, err := query("GOCACHE")
	if err != nil {
		return nil, err
	}
	home := filepath.Join(dir, "home")
	tmp := filepath.Join(dir, "tmp")
	for _, d := range []string{home, tmp} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"TMPDIR=" + tmp,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"GOROOT=" + goroot,
		"GOMODCACHE=" + modcache,
		"GOCACHE=" + gocache,
		"GOPATH=" + filepath.Join(home, "gopath"),
		"GOENV=off",
		"GOFLAGS=-mod=readonly",
		"GOPROXY=off",
		"GOSUMDB=off",
		"GOTOOLCHAIN=local",
		"GOTELEMETRY=off",
		"GOWORK=off",
	}
	if cc := os.Getenv("CC"); cc != "" {
		env = append(env, "CC="+cc)
	}
	return env, nil
}

func failOnAncestorPolicy(t *testing.T, root string) {
	t.Helper()
	dir := root
	for {
		for _, rel := range []string{"config/routing.yaml", "os/Clavain/config/routing.yaml", "os/clavain/config/routing.yaml"} {
			p := filepath.Join(dir, rel)
			if _, err := os.Stat(p); err == nil {
				t.Fatalf("routing policy %s exists above the temp root %s; working-directory discovery would find it and change the no-policy cases. Run with TMPDIR set to a directory outside that tree.", p, root)
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return
		}
		dir = parent
	}
}

const fixtureRel = "testdata/routing.yaml"

var fakeCLIs = []string{"bb", "gh", "codex", "claude", "kimi", "git", "curl", "ssh"}

type sandbox struct {
	t       *testing.T
	root    string
	home    string
	cwd     string
	bin     string
	ctxDir  string
	fakeLog string
	policy  string
	ctxN    int
	ids     []string
}

func newSandbox(t *testing.T) *sandbox {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	failOnAncestorPolicy(t, base)
	testutil.FailOnAncestorDB(t, base)
	s := &sandbox{t: t, root: base}
	s.home = filepath.Join(base, "home")
	s.cwd = filepath.Join(base, "work")
	s.bin = filepath.Join(base, "fakebin")
	s.ctxDir = filepath.Join(base, "ctx")
	s.fakeLog = filepath.Join(base, "fake-cli.log")
	s.policy = filepath.Join(base, "plugin", "config", "routing.yaml")
	for _, d := range []string{s.home, s.cwd, s.bin, s.ctxDir, filepath.Dir(s.policy), filepath.Join(base, "tmp")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range fakeCLIs {
		script := "#!/bin/sh\nprintf '%s %s\\n' \"$(basename \"$0\")\" \"$*\" >> \"$ORCHCOMPAT_FAKE_LOG\"\nexit 97\n"
		if err := os.WriteFile(filepath.Join(s.bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(fixtureRel)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.policy, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return s
}

func (s *sandbox) env(extra map[string]string) []string {
	env := map[string]string{
		"HOME":                 s.home,
		"PATH":                 s.bin + ":/usr/bin:/bin",
		"TMPDIR":               filepath.Join(s.root, "tmp"),
		"LANG":                 "C",
		"TZ":                   "UTC",
		"ORCHCOMPAT_FAKE_LOG":  s.fakeLog,
		"XDG_CONFIG_HOME":      filepath.Join(s.home, ".config"),
		"XDG_DATA_HOME":        filepath.Join(s.home, ".local", "share"),
		"GH_TOKEN":             "synthetic-not-a-token",
		"GIT_CONFIG_NOSYSTEM":  "1",
		"GIT_CONFIG_GLOBAL":    "/dev/null",
		"GIT_TERMINAL_PROMPT":  "0",
		"NO_COLOR":             "1",
		"ORCHCOMPAT_SANDBOXED": "1",
	}
	for k, v := range extra {
		if v == "" {
			delete(env, k)
			continue
		}
		env[k] = v
	}
	var out []string
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

type result struct {
	Code   int
	Stdout string
	Stderr string
}

func (s *sandbox) exec(env map[string]string, cwd string, args ...string) result {
	s.t.Helper()
	cmd := exec.Command(icBinary, args...)
	cmd.Dir = cwd
	cmd.Env = s.env(env)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		code = exitErr.ExitCode()
	default:
		s.t.Fatalf("running ic: %v", err)
	}
	return result{Code: code, Stdout: stdout.String(), Stderr: stderr.String()}
}

func (s *sandbox) snapshot() map[string]string {
	s.t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(s.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(s.root, p)
		if rel == "." || rel == filepath.Base(s.fakeLog) {
			return nil
		}
		if d.IsDir() {
			out[rel] = "dir"
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			target, _ := os.Readlink(p)
			out[rel] = "link:" + target
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		out[rel] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		s.t.Fatal(err)
	}
	return out
}

func diffSnapshots(before, after map[string]string) []string {
	var changes []string
	for k, v := range after {
		old, ok := before[k]
		switch {
		case !ok:
			changes = append(changes, "created "+k)
		case old != v:
			changes = append(changes, "modified "+k)
		}
	}
	for k := range before {
		if _, ok := after[k]; !ok {
			changes = append(changes, "removed "+k)
		}
	}
	sort.Strings(changes)
	return changes
}

func (s *sandbox) fakeCalls() string {
	data, err := os.ReadFile(s.fakeLog)
	if errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	if err != nil {
		s.t.Fatal(err)
	}
	return string(data)
}

type runcase struct {
	Label     string
	Args      []string
	Ctx       string
	Env       map[string]string
	Cwd       string
	WantFiles []string
	Post      func(string) string
}

var (
	timeField  = regexp.MustCompile(`"time":"[^"]*"`)
	backupName = regexp.MustCompile(`\.backup-(?:[0-9a-f]{32}-)?[0-9]+-[0-9]{8}-[0-9]{6}`)
	jobID      = regexp.MustCompile(`"job_id":"[0-9a-f]{32}"`)
	epochField = regexp.MustCompile(`"created_at":[0-9]+`)
)

func (s *sandbox) normalize(text string) string {
	text = strings.ReplaceAll(text, s.root, "<SANDBOX>")
	for _, id := range s.ids {
		text = strings.ReplaceAll(text, id, "<JOB-ID>")
	}
	text = timeField.ReplaceAllString(text, `"time":"<TIME>"`)
	text = backupName.ReplaceAllString(text, ".backup-<STAMP>")
	text = epochField.ReplaceAllString(text, `"created_at":<EPOCH>`)
	text = jobID.ReplaceAllString(text, `"job_id":"<JOB-ID>"`)
	return text
}

func (s *sandbox) expand(arg string) string {
	arg = strings.ReplaceAll(arg, "{POLICY}", s.policy)
	arg = strings.ReplaceAll(arg, "{ROOT}", s.root)
	arg = strings.ReplaceAll(arg, "{HOME}", s.home)
	arg = strings.ReplaceAll(arg, "{CWD}", s.cwd)
	return arg
}

func (s *sandbox) run(c runcase) (string, result) {
	s.t.Helper()
	args := make([]string, 0, len(c.Args)+1)
	for _, a := range c.Args {
		args = append(args, s.expand(a))
	}
	if c.Ctx != "" {
		s.ctxN++
		path := filepath.Join(s.ctxDir, fmt.Sprintf("context-%d.json", s.ctxN))
		if err := os.WriteFile(path, []byte(c.Ctx), 0o644); err != nil {
			s.t.Fatal(err)
		}
		args = append(args, "--context-file="+path)
	}
	env := map[string]string{}
	for k, v := range c.Env {
		env[k] = s.expand(v)
	}
	cwd := s.cwd
	if c.Cwd != "" {
		cwd = s.expand(c.Cwd)
	}
	before := s.snapshot()
	res := s.exec(env, cwd, args...)
	changes := diffSnapshots(before, s.snapshot())
	for i := range changes {
		changes[i] = backupName.ReplaceAllString(changes[i], ".backup-<STAMP>")
	}
	if !sameStrings(changes, c.WantFiles) {
		s.t.Errorf("%s: file effects = %v, want %v", c.Label, changes, c.WantFiles)
	}
	if calls := s.fakeCalls(); calls != "" {
		s.t.Errorf("%s: fake CLIs were invoked: %s", c.Label, calls)
	}
	return s.render(c, args, res, changes), res
}

func sameStrings(a, b []string) bool {
	a = append([]string(nil), a...)
	b = append([]string(nil), b...)
	sort.Strings(a)
	sort.Strings(b)
	return strings.Join(a, "\n") == strings.Join(b, "\n")
}

func (s *sandbox) render(c runcase, args []string, res result, changes []string) string {
	var b strings.Builder
	shown := make([]string, len(args))
	for i, a := range args {
		if strings.HasPrefix(a, "--context-file="+s.ctxDir) {
			a = "--context-file=<CTX>"
		}
		shown[i] = s.normalize(a)
	}
	fmt.Fprintf(&b, "=== %s\n", c.Label)
	fmt.Fprintf(&b, "$ ic %s\n", strings.Join(shown, " "))
	if len(c.Env) > 0 {
		var keys []string
		for k := range c.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "env: %s=%s\n", k, s.normalize(s.expand(c.Env[k])))
		}
	}
	if c.Ctx != "" {
		fmt.Fprintf(&b, "context: %s\n", strings.TrimSpace(c.Ctx))
	}
	fmt.Fprintf(&b, "exit: %d\n", res.Code)
	if len(changes) > 0 {
		fmt.Fprintf(&b, "files: %s\n", s.normalize(strings.Join(changes, ", ")))
	}
	stdout := res.Stdout
	if c.Post != nil {
		stdout = c.Post(stdout)
	}
	fmt.Fprintf(&b, "--- stdout\n%s", ensureNewline(s.normalize(stdout)))
	fmt.Fprintf(&b, "--- stderr\n%s\n", ensureNewline(s.normalize(res.Stderr)))
	return b.String()
}

func ensureNewline(text string) string {
	if text != "" && !strings.HasSuffix(text, "\n") {
		return text + "\n"
	}
	return text
}

type goldenSet struct {
	t     *testing.T
	name  string
	parts []string
}

func newGolden(t *testing.T, name string) *goldenSet {
	return &goldenSet{t: t, name: name}
}

func (g *goldenSet) add(text string) {
	g.parts = append(g.parts, text)
}

func (g *goldenSet) check() {
	g.t.Helper()
	got := strings.Join(g.parts, "\n")
	path := filepath.Join("testdata", "golden", g.name+".golden")
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			g.t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			g.t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		g.t.Fatalf("missing golden %s (run with -update): %v", path, err)
	}
	if string(want) == got {
		return
	}
	wl, gl := strings.Split(string(want), "\n"), strings.Split(got, "\n")
	for i := 0; i < len(wl) || i < len(gl); i++ {
		var w, gg string
		if i < len(wl) {
			w = wl[i]
		}
		if i < len(gl) {
			gg = gl[i]
		}
		if w != gg {
			g.t.Fatalf("golden %s differs at line %d\nwant: %s\n got: %s", path, i+1, w, gg)
		}
	}
}

func (s *sandbox) runAll(name string, cases []runcase) []result {
	s.t.Helper()
	g := newGolden(s.t, name)
	var results []result
	for _, c := range cases {
		text, res := s.run(c)
		g.add(text)
		results = append(results, res)
	}
	g.check()
	return results
}

type fixtureShape struct {
	Reasoning struct {
		FrontierReasons []string `yaml:"frontier_reasons"`
	} `yaml:"reasoning"`
	Dispatch struct {
		Roles map[string]string         `yaml:"roles"`
		Tiers map[string]map[string]any `yaml:"tiers"`
	} `yaml:"dispatch"`
}

func loadFixtureShape(t *testing.T) fixtureShape {
	t.Helper()
	data, err := os.ReadFile(fixtureRel)
	if err != nil {
		t.Fatal(err)
	}
	var shape fixtureShape
	if err := yaml.Unmarshal(data, &shape); err != nil {
		t.Fatal(err)
	}
	return shape
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
