package main

import (
	"strings"
	"testing"
	"time"
)

func TestBusyTimeoutDefaultAndRouting(t *testing.T) {
	for _, tc := range []struct {
		name        string
		args        []string
		subcommand  string
		subArgs     []string
		busyTimeout time.Duration
	}{
		{
			name:        "dispatch wait keeps its deadline",
			args:        []string{"dispatch", "wait", "abc", "--timeout=2s"},
			subcommand:  "dispatch",
			subArgs:     []string{"wait", "abc", "--timeout=2s"},
			busyTimeout: 5 * time.Second,
		},
		{
			name:        "timeout before the subcommand still reaches dispatch wait",
			args:        []string{"--timeout=2s", "dispatch", "wait", "abc"},
			subcommand:  "dispatch",
			subArgs:     []string{"wait", "abc", "--timeout=2s"},
			busyTimeout: 5 * time.Second,
		},
		{
			name:        "dispatch spawn keeps its worker timeout",
			args:        []string{"dispatch", "spawn", "--prompt-file=p", "--timeout=30s"},
			subcommand:  "dispatch",
			subArgs:     []string{"spawn", "--prompt-file=p", "--timeout=30s"},
			busyTimeout: 5 * time.Second,
		},
		{
			name:        "lock acquire keeps its wait",
			args:        []string{"lock", "acquire", "name", "scope", "--timeout=200ms"},
			subcommand:  "lock",
			subArgs:     []string{"acquire", "name", "scope", "--timeout=200ms"},
			busyTimeout: 5 * time.Second,
		},
		{
			name:        "other commands keep timeout as the busy timeout",
			args:        []string{"run", "status", "r1", "--timeout=2s"},
			subcommand:  "run",
			subArgs:     []string{"status", "r1"},
			busyTimeout: 2 * time.Second,
		},
		{
			name:        "other dispatch subcommands keep timeout as the busy timeout",
			args:        []string{"dispatch", "list", "--timeout=2s"},
			subcommand:  "dispatch",
			subArgs:     []string{"list"},
			busyTimeout: 2 * time.Second,
		},
		{
			name:        "busy timeout flag applies alongside a routed timeout",
			args:        []string{"--busy-timeout=250ms", "dispatch", "wait", "abc", "--timeout=2s"},
			subcommand:  "dispatch",
			subArgs:     []string{"wait", "abc", "--timeout=2s"},
			busyTimeout: 250 * time.Millisecond,
		},
		{
			name:        "explicit busy timeout wins over the legacy global timeout",
			args:        []string{"run", "status", "r1", "--timeout=2s", "--busy-timeout=750ms"},
			subcommand:  "run",
			subArgs:     []string{"status", "r1"},
			busyTimeout: 750 * time.Millisecond,
		},
		{
			name:        "routed timeout is validated by its command, not here",
			args:        []string{"dispatch", "wait", "abc", "--timeout=soon"},
			subcommand:  "dispatch",
			subArgs:     []string{"wait", "abc", "--timeout=soon"},
			busyTimeout: 5 * time.Second,
		},
		{
			name:        "busy timeout defaults to five seconds",
			args:        []string{"health"},
			subcommand:  "health",
			busyTimeout: 5 * time.Second,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, err := parseGlobalArgs(tc.args)
			if err != nil {
				t.Fatalf("parseGlobalArgs(%q): %v", tc.args, err)
			}
			if g.subcommand != tc.subcommand {
				t.Errorf("subcommand = %q, want %q", g.subcommand, tc.subcommand)
			}
			if strings.Join(g.subArgs, "\x00") != strings.Join(tc.subArgs, "\x00") {
				t.Errorf("subArgs = %q, want %q", g.subArgs, tc.subArgs)
			}
			if g.busyTimeout != tc.busyTimeout {
				t.Errorf("busyTimeout = %v, want %v", g.busyTimeout, tc.busyTimeout)
			}
		})
	}
}

func TestParseGlobalArgsRejectsInvalidGlobalDurations(t *testing.T) {
	for _, args := range [][]string{
		{"--busy-timeout=soon", "health"},
		{"health", "--timeout=soon"},
	} {
		if _, err := parseGlobalArgs(args); err == nil {
			t.Errorf("parseGlobalArgs(%q) accepted an invalid duration", args)
		}
	}
}
