package main

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
)

func captureRouteStderr(t *testing.T, fn func() int) (int, string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	code := fn()
	_ = w.Close()
	os.Stderr = old
	out, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil {
		t.Fatal(err)
	}
	return code, string(out)
}

// A misspelt label that is shaped like a label must not route to a
// vendor-cloud deployment: it is rejected, with the known labels named.
func TestRouteDecideRejectsUnknownDataLabel(t *testing.T) {
	vendorOnly := writeRegistry(t, vendorOnlyRegistry)
	for _, label := range []string{"client-confidential-", "client-confidentail", "clientconfidential", "confidential"} {
		args := []string{"--class=synthesis", "--role=researcher", "--data=" + label, "--registry=" + vendorOnly}
		var code int
		var out []byte
		code, errOut := captureRouteStderr(t, func() int {
			c, o := captureRouteStdout(t, func() int { return cmdRouteDecide(context.Background(), args) })
			out = o
			return c
		})
		if code != exitMalformed {
			t.Errorf("--data=%s: code %d, want %d", label, code, exitMalformed)
		}
		if len(out) != 0 {
			t.Errorf("--data=%s: routed to %q, want no decision", label, out)
		}
		if !strings.Contains(errOut, "known sensitivity label") || !strings.Contains(errOut, "client-confidential") {
			t.Errorf("--data=%s: stderr %q should name the known labels", label, errOut)
		}
	}
}

// The decision and the constraint-violation reason are asserted on output,
// not only on exit codes.
func TestRouteDecideOutputs(t *testing.T) {
	mixed := writeRegistry(t, mixedRegistry)
	vendorOnly := writeRegistry(t, vendorOnlyRegistry)

	code, out := captureRouteStdout(t, func() int {
		return cmdRouteDecide(context.Background(), []string{"--class=synthesis", "--role=researcher", "--data=client-confidential", "--registry=" + mixed})
	})
	if code != exitDecision || strings.TrimSpace(string(out)) != "nousresearch/hermes-4@localhost-gpu" {
		t.Errorf("confidential with local: code=%d out=%q, want the local deployment", code, out)
	}

	code, errOut := captureRouteStderr(t, func() int {
		return cmdRouteDecide(context.Background(), []string{"--class=synthesis", "--role=researcher", "--data=client-confidential", "--registry=" + vendorOnly})
	})
	if code != exitConstraintViolation || !strings.Contains(errOut, "constraint-violation") {
		t.Errorf("confidential with vendor only: code=%d stderr=%q", code, errOut)
	}

	for _, label := range []string{"public", "internal"} {
		code, out = captureRouteStdout(t, func() int {
			return cmdRouteDecide(context.Background(), []string{"--class=x", "--role=executor", "--data=" + label, "--registry=" + vendorOnly})
		})
		if code != exitDecision || strings.TrimSpace(string(out)) != "anthropic/claude-fable-5@api" {
			t.Errorf("--data=%s: code=%d out=%q, want the unconstrained decision", label, code, out)
		}
	}
}

// An explicitly empty boolean value is malformed, never affirmative evidence.
func TestRouteRecordEvidenceRejectsEmptyBool(t *testing.T) {
	for _, bad := range [][]string{
		{"--kind=gate", "--gate=g", "--model=m", "--executed="},
		{"--kind=escalation", "--from=sonnet", "--to=opus", "--exhausted="},
	} {
		code, errOut := captureRouteStderr(t, func() int {
			return cmdRouteRecordEvidence(context.Background(), bad)
		})
		if code != exitMalformed || !strings.Contains(errOut, "empty value") {
			t.Errorf("%v: code=%d stderr=%q, want malformed naming the empty value", bad, code, errOut)
		}
	}
}
