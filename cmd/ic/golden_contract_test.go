package main

// Golden output contract for the ic subcommands that other repositories read
// (bead mk-ck9ig). The test runs the real ic entry point against a seeded
// temporary project, normalizes the values that change per run, and compares
// the result with contracts/golden. Any drift fails until the goldens are
// regenerated with -update-goldens and the diff is reviewed:
//
//	go test ./cmd/ic -run TestJSONOutputGoldens -update-goldens
//
// See contracts/golden/README.md for how reader repositories consume them.

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

var updateGoldens = flag.Bool("update-goldens", false, "regenerate contracts/golden from real ic output")

const (
	goldenDir       = "../../contracts/golden"
	goldenExecMain  = "IC_GOLDEN_EXEC_MAIN"
	goldenTimestamp = 1700000000
	// A timestamp at or above goldenMaxSeconds is rejected: in seconds it is
	// the year 2286, so it is almost certainly milliseconds.
	goldenMaxSeconds = 10_000_000_000
	goldenDate       = "2000-01-01"
	goldenProjectDir = "/PROJECT"
)

// TestMain lets the golden test run this test binary as the real ic CLI.
func TestMain(m *testing.M) {
	if os.Getenv(goldenExecMain) == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// goldenCase is one reader-facing command. Args use placeholders that are
// substituted with the seeded IDs before the command runs.
type goldenCase struct {
	File   string   `json:"file"`
	Format string   `json:"format"` // "json" or "text"
	Args   []string `json:"argv"`
}

// runCreateArgs creates the seeded run; its output is the run-create golden.
func runCreateArgs(dir string) []string {
	return []string{"--json", "run", "create", "--project=" + dir, "--goal=golden goal",
		"--scope-id=bead-1", "--token-budget=100000"}
}

var goldenCases = []goldenCase{
	{"route-model.json", "json", []string{"--json", "route", "model", "--policy=POLICY", "--phase=executing"}},
	{"run-status.json", "json", []string{"--json", "run", "status", "RUN_ID"}},
	{"run-phase.txt", "text", []string{"--json", "run", "phase", "RUN_ID"}},
	{"run-events.json", "json", []string{"--json", "run", "events", "RUN_ID"}},
	{"run-budget.json", "json", []string{"--json", "run", "budget", "RUN_ID"}},
	{"run-budget-unset.json", "json", []string{"--json", "run", "budget", "BARE_RUN_ID"}},
	{"run-agent-list.json", "json", []string{"--json", "run", "agent", "list", "RUN_ID"}},
	{"run-artifact-list.json", "json", []string{"--json", "run", "artifact", "list", "RUN_ID"}},
	{"run-action-list.json", "json", []string{"--json", "run", "action", "list", "RUN_ID"}},
	{"goal-list.json", "json", []string{"--json", "goal", "list"}},
	{"cost-baseline.json", "json", []string{"--json", "cost", "baseline", "--script=COST_SCRIPT"}},
	{"dispatch-status.json", "json", []string{"--json", "dispatch", "status", "DISPATCH_ID"}},
	{"dispatch-list.json", "json", []string{"--json", "dispatch", "list"}},
	{"lane-status.json", "json", []string{"--json", "lane", "status", "golden-lane"}},
}

// goldenEnv runs ic with this test binary in dir.
type goldenEnv struct {
	t         *testing.T
	dir       string
	env       []string
	lastOut   string // stdout of the latest icID call
	createOut string // stdout of the run create that seeded RUN_ID
}

func newGoldenEnv(t *testing.T) *goldenEnv {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(dir, ".home")
	if err := os.Mkdir(home, 0o755); err != nil {
		t.Fatal(err)
	}
	// A minimal environment keeps caller state (trace IDs, routing policy,
	// plugin roots) out of the output.
	env := []string{goldenExecMain + "=1", "HOME=" + home, "PATH=" + os.Getenv("PATH"), "TZ=UTC"}
	return &goldenEnv{t: t, dir: dir, env: env}
}

func (g *goldenEnv) ic(args ...string) (string, int) {
	g.t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Dir = g.dir
	cmd.Env = g.env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		g.t.Fatalf("ic %v: %v", args, err)
	}
	if code != 0 {
		g.t.Fatalf("ic %v: exit %d\nstdout: %s\nstderr: %s", args, code, stdout.String(), stderr.String())
	}
	return stdout.String(), code
}

func (g *goldenEnv) icID(args ...string) string {
	g.t.Helper()
	out, _ := g.ic(args...)
	g.lastOut = out
	var v struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil || len(v.ID) == 0 {
		g.t.Fatalf("ic %v: no id in %q: %v", args, out, err)
	}
	return strings.Trim(string(v.ID), `"`)
}

func (g *goldenEnv) write(name, content string, mode os.FileMode) string {
	g.t.Helper()
	path := filepath.Join(g.dir, name)
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		g.t.Fatal(err)
	}
	return path
}

// seed creates one of everything the goldens read and returns the
// placeholder for each generated value.
func (g *goldenEnv) seed() map[string]string {
	t := g.t
	t.Helper()
	ids := map[string]string{}

	ids["POLICY"] = g.write("routing.yaml", `subagents:
  defaults:
    model: sonnet
  phases:
    executing:
      model: opus
`, 0o644)
	ids["COST_SCRIPT"] = g.write("cost-query.sh", `case "$1" in
by-bead) echo '[{"bead_id":"bead-1","runs":2,"tokens":3000,"input_tokens":2000,"output_tokens":1000}]' ;;
*) echo '[]' ;;
esac
`, 0o755)

	ids["RUN_ID"] = g.icID(runCreateArgs(g.dir)...)
	g.createOut = g.lastOut
	ids["BARE_RUN_ID"] = g.icID("--json", "run", "create", "--project="+g.dir, "--goal=bare run")
	run := ids["RUN_ID"]
	ids["AGENT_ID"] = g.icID("--json", "run", "agent", "add", run, "--type=claude", "--name=worker-1")
	ids["ARTIFACT_ID"] = g.icID("--json", "run", "artifact", "add", run, "--phase=brainstorm",
		"--path=docs/plan.md", "--type=plan")
	g.ic("--json", "run", "action", "add", run, "--phase=executing", "--command=/clavain:work")
	g.ic("--json", "run", "advance", run)
	ids["GOAL_ID"] = g.icID("--json", "goal", "create", "--title=golden goal", "--project="+g.dir,
		"--condition=tests pass or stop after 5 turns")
	ids["LANE_ID"] = g.icID("--json", "lane", "create", "--name=golden-lane", "--type=standing",
		"--description=golden lane")
	g.ic("--json", "lane", "sync", "golden-lane", "--bead-ids=bead-1")
	g.ic("--json", "landed", "record", "--commit=abc123", "--project="+g.dir, "--branch=main",
		"--bead=bead-1", "--run="+run, "--files=2", "--insertions=10", "--deletions=3")

	// ic has no command that records a finished dispatch without spawning a
	// process, so the row is written directly. The output shape under test
	// is still produced by ic's dispatch status and list.
	ids["DISPATCH_ID"] = "golden-dispatch"
	db, err := sql.Open("sqlite", filepath.Join(g.dir, ".clavain", "intercore.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO dispatches (id, agent_type, status, project_dir, name, model,
		turns, input_tokens, output_tokens, exit_code, scope_id, created_at, started_at, completed_at,
		verdict_status, verdict_summary)
		VALUES (?, 'codex', 'completed', ?, 'reviewer', 'gpt-test', 3, 1200, 340, 0, ?, ?, ?, ?, 'pass', 'ok')`,
		ids["DISPATCH_ID"], g.dir, run, goldenTimestamp+1, goldenTimestamp+2, goldenTimestamp+3); err != nil {
		t.Fatal(err)
	}
	return ids
}

var (
	goldenDateRe   = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	goldenDigitsRe = regexp.MustCompile(`\d{10,}`)
)

// normalizeValue replaces wall-clock values so the goldens are stable.
// Types are kept: a timestamp stays a number, a date stays a string. A
// timestamp must be an integer count of Unix seconds; anything else (a
// fraction, milliseconds, a negative value) is reported in bad rather than
// normalized, so a change of unit cannot hide behind the placeholder.
func normalizeValue(key string, v any, bad *[]string) any {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			x[k] = normalizeValue(k, child, bad)
		}
		return x
	case []any:
		for i, child := range x {
			x[i] = normalizeValue(key, child, bad)
		}
		return x
	case json.Number:
		if strings.HasSuffix(key, "_at") || strings.HasSuffix(key, "At") {
			n, err := strconv.ParseInt(x.String(), 10, 64)
			if err != nil || n < 0 || n >= goldenMaxSeconds {
				*bad = append(*bad, fmt.Sprintf("%s=%s is not Unix seconds", key, x))
				return v
			}
			if n == 0 {
				return v
			}
			return json.Number(fmt.Sprint(goldenTimestamp))
		}
	case string:
		if goldenDateRe.MatchString(x) {
			return goldenDate
		}
		if key == "span_id" || key == "parent_span_id" {
			return goldenDigitsRe.ReplaceAllString(x, "0")
		}
	}
	return v
}

// goldenReplacer maps placeholders to seeded values, or the reverse. Longer
// strings come first so the result does not depend on map order.
func goldenReplacer(ids map[string]string, dir string, reverse bool) *strings.Replacer {
	var from []string
	for k, v := range ids {
		if !reverse {
			from = append(from, k)
		} else if !strings.HasPrefix(v, dir) {
			from = append(from, v)
		}
	}
	sort.Slice(from, func(i, j int) bool {
		if len(from[i]) != len(from[j]) {
			return len(from[i]) > len(from[j])
		}
		return from[i] < from[j]
	})
	inverse := map[string]string{}
	for k, v := range ids {
		inverse[v] = k
	}
	var pairs []string
	for _, f := range from {
		if reverse {
			pairs = append(pairs, f, inverse[f])
		} else {
			pairs = append(pairs, f, ids[f])
		}
	}
	return strings.NewReplacer(pairs...)
}

func normalizeGolden(t *testing.T, c goldenCase, out string, ids map[string]string, dir string) []byte {
	t.Helper()
	out = goldenReplacer(ids, dir, true).Replace(out)
	out = strings.ReplaceAll(out, dir, goldenProjectDir)
	if c.Format == "text" {
		return []byte(out)
	}
	b, err := normalizeJSON(out)
	if err != nil {
		t.Fatalf("%s: %v\n%s", c.File, err, out)
	}
	return b
}

// normalizeJSON requires out to be exactly one JSON value and returns it
// normalized and pretty-printed with sorted keys.
func normalizeJSON(out string) ([]byte, error) {
	dec := json.NewDecoder(strings.NewReader(out))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("ic output is not JSON: %w", err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("ic output has trailing data after its JSON value: %v", err)
	}
	var bad []string
	v = normalizeValue("", v, &bad)
	if len(bad) > 0 {
		return nil, fmt.Errorf("%s", strings.Join(bad, "; "))
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// TestNormalizeJSONRejects checks that normalization cannot hide malformed
// output or a change of timestamp unit.
func TestNormalizeJSONRejects(t *testing.T) {
	for _, out := range []string{
		`{"id":"x"}]`,
		`{"id":"x"} {"id":"y"}`,
		`{"created_at":1700000000123}`,
		`{"created_at":1700000000.5}`,
		`{"created_at":-1}`,
		`[{"UpdatedAt":"1700000000"}, {"UpdatedAt":1e9}]`,
	} {
		if b, err := normalizeJSON(out); err == nil {
			t.Errorf("normalizeJSON(%s) = %s, want an error", out, b)
		}
	}
	got, err := normalizeJSON("{\"created_at\":1760000000,\"closed_at\":0}\n")
	if err != nil {
		t.Fatal(err)
	}
	if want := "{\n  \"closed_at\": 0,\n  \"created_at\": 1700000000\n}\n"; string(got) != want {
		t.Errorf("normalizeJSON = %q, want %q", got, want)
	}
}

func TestJSONOutputGoldens(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the ic binary")
	}
	g := newGoldenEnv(t)

	g.ic("init")
	// goal list on an empty store is captured before seeding.
	emptyOut, _ := g.ic("--json", "goal", "list")
	ids := g.seed()

	// Two goldens come from outside the case table: goal list before any
	// goal exists, and the output of the run create that seeded RUN_ID.
	createArgs := runCreateArgs(goldenProjectDir)
	index := []goldenCase{
		{"goal-list-empty.json", "json", []string{"--json", "goal", "list"}},
		{"run-create.json", "json", createArgs},
	}
	got := map[string][]byte{
		"goal-list-empty.json": normalizeGolden(t, index[0], emptyOut, ids, g.dir),
		"run-create.json":      normalizeGolden(t, index[1], g.createOut, ids, g.dir),
	}
	expand := goldenReplacer(ids, g.dir, false)
	for _, c := range goldenCases {
		args := make([]string, len(c.Args))
		for i, a := range c.Args {
			args[i] = expand.Replace(a)
		}
		out, _ := g.ic(args...)
		got[c.File] = normalizeGolden(t, c, out, ids, g.dir)
		index = append(index, c)
	}
	sort.Slice(index, func(i, j int) bool { return index[i].File < index[j].File })
	indexJSON, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got["index.json"] = append(indexJSON, '\n')

	if *updateGoldens {
		if err := os.MkdirAll(goldenDir, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, b := range got {
			if err := os.WriteFile(filepath.Join(goldenDir, name), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	const hint = "if this change to ic output is intended, run: go test ./cmd/ic -run TestJSONOutputGoldens -update-goldens, then review the diff and update the readers listed in contracts/golden/README.md"
	entries, err := os.ReadDir(goldenDir)
	if err != nil {
		t.Fatalf("read %s: %v (%s)", goldenDir, err, hint)
	}
	for _, e := range entries {
		if e.Name() == "README.md" {
			continue
		}
		if _, ok := got[e.Name()]; !ok {
			if *updateGoldens {
				if err := os.Remove(filepath.Join(goldenDir, e.Name())); err != nil {
					t.Fatal(err)
				}
				continue
			}
			t.Errorf("%s: golden has no generating case; remove it (%s)", e.Name(), hint)
		}
	}
	names := make([]string, 0, len(got))
	for name := range got {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		want, err := os.ReadFile(filepath.Join(goldenDir, name))
		if err != nil {
			t.Errorf("%s: missing golden (%s)", name, hint)
			continue
		}
		if !bytes.Equal(want, got[name]) {
			t.Errorf("%s: ic output drifted from the golden (%s)\n--- golden\n%s\n--- ic\n%s", name, hint, want, got[name])
		}
	}
}
