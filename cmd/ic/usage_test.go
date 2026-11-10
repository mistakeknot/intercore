package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// dispatchedCommands returns every command name in the `switch subcommand`
// of main() in main.go.
func dispatchedCommands(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	var cmds []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "main" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			sw, ok := n.(*ast.SwitchStmt)
			if !ok {
				return true
			}
			if id, ok := sw.Tag.(*ast.Ident); !ok || id.Name != "subcommand" {
				return true
			}
			for _, stmt := range sw.Body.List {
				cc := stmt.(*ast.CaseClause)
				for _, e := range cc.List {
					if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
						name, _ := strconv.Unquote(lit.Value)
						cmds = append(cmds, name)
					}
				}
			}
			return false
		})
	}
	if len(cmds) < 20 {
		t.Fatalf("found only %d commands in main(); the switch moved or changed shape", len(cmds))
	}
	return cmds
}

// TestUsageListsEveryCommand fails when a command is wired into main() but
// missing from the text `ic` prints, so help cannot drift from the switch.
func TestUsageListsEveryCommand(t *testing.T) {
	usage := usageText()
	for _, cmd := range dispatchedCommands(t) {
		re := regexp.MustCompile(`(?m)^  ` + regexp.QuoteMeta(cmd) + `(\s|$)`)
		if !re.MatchString(usage) {
			t.Errorf("command %q is dispatched in main() but has no line in the usage text", cmd)
		}
	}
	if strings.Contains(usage, "\t") {
		t.Errorf("usage text contains a tab; use spaces")
	}
}

// TestCLIReferenceListsEveryCommand does the same for agents/cli-reference.md,
// which agents read to find commands.
func TestCLIReferenceListsEveryCommand(t *testing.T) {
	raw, err := os.ReadFile("../../agents/cli-reference.md")
	if err != nil {
		t.Fatalf("read cli-reference.md: %v", err)
	}
	ref := string(raw)
	for _, cmd := range dispatchedCommands(t) {
		re := regexp.MustCompile(`(?m)^ic ` + regexp.QuoteMeta(cmd) + `(\s|$)`)
		if !re.MatchString(ref) {
			t.Errorf("command %q is dispatched in main() but agents/cli-reference.md has no `ic %s` line", cmd, cmd)
		}
	}
}
