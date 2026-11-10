package orchcompat

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func spawnArgs(extra ...string) []string {
	return append([]string{"--json", "dispatch", "spawn", "--policy={POLICY}"}, extra...)
}

func TestDispatchSpawnRoleErrors(t *testing.T) {
	s := newSandbox(t)
	prompt := filepath.Join(s.root, "prompt.md")
	if err := os.WriteFile(prompt, []byte("synthetic prompt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reasons := `{"reasons":["capability-failure"],"rationale":"synthetic characterization rationale"}`
	cases := []runcase{
		{Label: "role with model", Args: spawnArgs("--role=planning", "--model=gpt-6-astra", "--prompt-file="+prompt)},
		{Label: "role with type", Args: spawnArgs("--role=planning", "--type=codex", "--prompt-file="+prompt)},
		{Label: "role missing policy", Args: []string{"--json", "dispatch", "spawn", "--policy={ROOT}/absent.yaml", "--role=planning", "--prompt-file=" + prompt}},
		{Label: "role no policy selected", Args: []string{"--json", "dispatch", "spawn", "--role=planning", "--prompt-file=" + prompt}},
		{Label: "role malformed context", Args: spawnArgs("--role=planning", "--prompt-file="+prompt), Ctx: "not json"},
		{Label: "role blank rationale", Args: spawnArgs("--role=planning", "--prompt-file="+prompt), Ctx: `{"reasons":["capability-failure"],"rationale":""}`},
		{Label: "role unsupported", Args: spawnArgs("--role=not-a-role", "--prompt-file="+prompt)},
		{Label: "role review without producer", Args: spawnArgs("--role=validation", "--prompt-file="+prompt)},
		{Label: "role missing prompt file flag", Args: spawnArgs("--role=planning")},
		{Label: "role missing prompt file on disk", Args: spawnArgs("--role=planning", "--prompt-file={ROOT}/absent.md", "--scheduled")},
		{Label: "role effort floor still resolves", Args: spawnArgs("--role=planning", "--prompt-file="+prompt, "--run-id="), Ctx: reasons},
		{Label: "role bad limit", Args: spawnArgs("--role=planning", "--prompt-file="+prompt, "--max-active-global=-1")},
		{Label: "role bad timeout", Args: spawnArgs("--role=planning", "--prompt-file="+prompt, "--timeout=soon")},
	}
	s.runAll("dispatch-spawn-role-errors", cases)
}

func TestDispatchSpawnRoleScheduled(t *testing.T) {
	s := newSandbox(t)
	prompt := filepath.Join(s.root, "prompt.md")
	if err := os.WriteFile(prompt, []byte("synthetic prompt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(s.cwd, "t.db")
	g := newGolden(t, "dispatch-spawn-role-scheduled")

	initText, initRes := s.run(runcase{Label: "init", Args: []string{"--db=" + db, "init"}, WantFiles: []string{"created work/t.db", "created work/t.db.backup-<STAMP>.db"}})
	g.add(initText)
	if initRes.Code != 0 {
		t.Fatalf("init failed: %d %s", initRes.Code, initRes.Stderr)
	}

	type spawnCase struct {
		label, role string
		ctx         string
		extra       []string
	}
	spawns := []spawnCase{
		{label: "planning no context", role: "planning"},
		{label: "planning capability-failure", role: "planning", ctx: contextForReason("capability-failure")},
		{label: "routine-execution foundational", role: "routine-execution", ctx: contextForReason("foundational-invariants")},
		{label: "validation with producer", role: "validation", extra: []string{"--producer-identity=claude-opus-5-5"}},
		{label: "planning with effort in session", role: "planning", extra: []string{"--scheduler-session=synthetic-session"}},
	}
	for _, sc := range spawns {
		args := []string{"--db=" + db, "--json", "dispatch", "spawn", "--policy={POLICY}", "--role=" + sc.role, "--prompt-file=" + prompt, "--project={CWD}", "--scheduled"}
		args = append(args, sc.extra...)
		want := []string{"modified work/t.db"}
		text, res := s.run(runcase{Label: "spawn " + sc.label, Args: args, Ctx: sc.ctx, WantFiles: want})
		if res.Code != 0 {
			t.Fatalf("spawn %s: exit %d: %s", sc.label, res.Code, res.Stderr)
		}
		var out struct {
			JobID     string `json:"job_id"`
			Scheduled bool   `json:"scheduled"`
		}
		if err := json.Unmarshal([]byte(res.Stdout), &out); err != nil || out.JobID == "" || !out.Scheduled {
			t.Fatalf("spawn %s: unexpected stdout %q", sc.label, res.Stdout)
		}
		s.ids = append(s.ids, out.JobID)
		g.add(text)
		statusText, statusRes := s.run(runcase{Label: "status " + sc.label, Args: []string{"--db=" + db, "--json", "scheduler", "status", out.JobID}})
		if statusRes.Code != 0 {
			t.Fatalf("status %s: exit %d: %s", sc.label, statusRes.Code, statusRes.Stderr)
		}
		g.add(statusText)
	}
	g.add(s.storedJobs(t, db))
	g.check()
}

func (s *sandbox) storedJobs(t *testing.T, db string) string {
	t.Helper()
	conn, err := sql.Open("sqlite", "file:"+db+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var b strings.Builder
	b.WriteString("=== stored scheduler jobs (read-only query, in submission order)\n")
	for _, id := range s.ids {
		var agentType, status, opts string
		row := conn.QueryRow("SELECT agent_type, status, spawn_opts FROM scheduler_jobs WHERE id = ?", id)
		if err := row.Scan(&agentType, &status, &opts); err != nil {
			t.Fatal(err)
		}
		var pretty map[string]any
		if err := json.Unmarshal([]byte(opts), &pretty); err != nil {
			t.Fatal(err)
		}
		canonical, err := json.MarshalIndent(pretty, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "job agent_type=%s status=%s\n%s\n", agentType, status, canonical)
	}
	return s.normalize(b.String())
}
