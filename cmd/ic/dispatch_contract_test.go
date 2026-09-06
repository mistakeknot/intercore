package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// These fixtures describe the CLI contract, rather than the internal storage
// struct. Exercise each command through SQLite and stdout to catch DTO drift.
func TestDispatchJSONFixtures(t *testing.T) {
	fixtureDir, err := filepath.Abs("../../contracts/testdata")
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []string{"sparse", "populated"} {
		t.Run(fixture, func(t *testing.T) {
			wantData, err := os.ReadFile(filepath.Join(fixtureDir, "dispatch-"+fixture+".json"))
			if err != nil {
				t.Fatal(err)
			}
			var want map[string]any
			if err := json.Unmarshal(wantData, &want); err != nil {
				t.Fatal(err)
			}
			setupCommandMetadataDB(t)
			flagJSON = true
			d, err := openDB()
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			_, err = d.SqlDB().Exec(`INSERT INTO dispatches
				(id, agent_type, status, project_dir, created_at)
				VALUES ('fixture1', 'flere', 'spawned', '/fixture/project', 1700000000)`)
			if err != nil {
				t.Fatal(err)
			}
			if fixture == "populated" {
				_, err = d.SqlDB().Exec(`UPDATE dispatches SET
					status = 'completed', prompt_file = '/fixture/prompt.md', output_file = '/fixture/output.md',
					pid = 123, exit_code = 0, name = '', model = 'fixture-model',
					turns = 2, commands = 3, messages = 4, input_tokens = 5, output_tokens = 6, cache_hits = 0,
					started_at = 1700000001, completed_at = 1700000002,
					verdict_status = 'pass', verdict_summary = 'fixture completed', error_message = '', quarantine_reason = '',
					sandbox_spec = '{"mode":"read-only","network":false}',
					sandbox_effective = '{"mode":"read-only","paths":["/fixture/project"]}',
					scope_id = 'fixture-run', parent_id = 'fixture-parent'
					WHERE id = 'fixture1'`)
				if err != nil {
					t.Fatal(err)
				}
			}
			commands := []string{"status", "list"}
			if fixture == "populated" {
				commands = append(commands, "poll", "wait")
			}
			for _, command := range commands {
				t.Run(command, func(t *testing.T) {
					args := []string{command, "fixture1"}
					if command == "list" {
						args = []string{command}
					}
					out := captureDispatchOutput(t, func() int {
						return cmdDispatch(context.Background(), args)
					})
					var got map[string]any
					if command == "list" {
						var items []map[string]any
						if err := json.Unmarshal(out, &items); err != nil {
							t.Fatal(err)
						}
						if len(items) != 1 {
							t.Fatalf("list returned %d items, want 1", len(items))
						}
						got = items[0]
					} else if err := json.Unmarshal(out, &got); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("CLI output differs from fixture\ngot: %s\nwant: %s", out, wantData)
					}
				})
			}
		})
	}
}

func captureDispatchOutput(t *testing.T, command func() int) []byte {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	oldStdout := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = oldStdout }()
	rc := command()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if rc != 0 {
		t.Fatalf("command rc = %d, want 0; stdout = %s", rc, out)
	}
	return out
}
