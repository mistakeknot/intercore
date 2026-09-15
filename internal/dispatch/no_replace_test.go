package dispatch

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A REPLACE deletes and reinserts the row. On dispatches and dispatch_terminals
// that overrides the conflict clauses inside the terminal triggers and skips the
// update-only guards (contract §4, probe T4); on the supervision, intent,
// consumption and delivery tables it silently overwrites a recorded claim or
// audit row. Every write to these tables must be a plain UPDATE or INSERT.
const dispatchTables = `dispatch(es|_terminals|_supervision|_intents|_consumptions|_terminal_deliveries)\b`

var replaceWrite = regexp.MustCompile(`(?i)\bOR\s+REPLACE\s+(INTO\s+)?` + dispatchTables + `|\bREPLACE\s+INTO\s+` + dispatchTables)

func TestNoReplaceOnDispatches(t *testing.T) {
	for _, stmt := range []string{
		"REPLACE INTO dispatches (id) VALUES (?)",
		"INSERT OR REPLACE INTO dispatch_terminals VALUES (?)",
		"update or replace dispatches set status = ?",
		"INSERT OR REPLACE INTO dispatch_consumptions (consumer) VALUES (?)",
		"REPLACE INTO dispatch_terminal_deliveries (consumer) VALUES (?)",
	} {
		if !replaceWrite.MatchString(stmt) {
			t.Fatalf("pattern misses %q", stmt)
		}
	}

	root := filepath.Join("..", "..")
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root not found: %v", err)
	}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".claude", "docs", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, "_test.go") || !(strings.HasSuffix(path, ".go") || strings.HasSuffix(path, ".sql")) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if loc := replaceWrite.FindIndex(data); loc != nil {
			line := 1 + strings.Count(string(data[:loc[0]]), "\n")
			t.Errorf("%s:%d: REPLACE write to a dispatch table", path, line)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
