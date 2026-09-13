package publish

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestGitRemoteSlugParsesGitHubForms(t *testing.T) {
	cases := []struct {
		url, owner, repo string
		ok               bool
	}{
		{"https://github.com/mistakeknot/interagency-marketplace.git", "mistakeknot", "interagency-marketplace", true},
		{"https://github.com/mistakeknot/interagency-marketplace", "mistakeknot", "interagency-marketplace", true},
		{"git@github.com:mistakeknot/intercore.git", "mistakeknot", "intercore", true},
		{"ssh://git@github.com/owner/repo.git", "owner", "repo", true},
		{"https://gitlab.com/owner/repo.git", "", "", false},
		{"/srv/git/local.git", "", "", false},
	}
	for _, c := range cases {
		m := githubRemoteRE.FindStringSubmatch(c.url)
		if !c.ok {
			if m != nil {
				t.Errorf("%s: expected no match, got %v", c.url, m)
			}
			continue
		}
		if m == nil {
			t.Errorf("%s: expected a match", c.url)
			continue
		}
		if m[1] != c.owner || m[2] != c.repo {
			t.Errorf("%s: got %s/%s, want %s/%s", c.url, m[1], m[2], c.owner, c.repo)
		}
	}
}

// The whole point of the gate is that it does not advance the branch when a
// required check failed.
func TestAwaitChecksFailsOnFailedCheck(t *testing.T) {
	fetch := func() (map[string]checkState, error) {
		return map[string]checkState{
			"structural": {done: true, successful: false, detailsURL: "https://example/run/1"},
		}, nil
	}
	err := awaitChecks(fetch, "abcdef123456", []string{"structural"},
		time.Minute, time.Minute, time.Millisecond, "publish-candidate/abcdef123456")
	if !errors.Is(err, ErrGateFailed) {
		t.Fatalf("want ErrGateFailed, got %v", err)
	}
	if !strings.Contains(err.Error(), "https://example/run/1") {
		t.Errorf("error should point at the failing run, got: %v", err)
	}
}

func TestAwaitChecksPassesWhenAllRequiredSucceed(t *testing.T) {
	fetch := func() (map[string]checkState, error) {
		return map[string]checkState{
			"structural": {done: true, successful: true},
			"sources":    {done: false}, // not required; must not block
		}, nil
	}
	if err := awaitChecks(fetch, "abcdef123456", []string{"structural"},
		time.Minute, time.Minute, time.Millisecond, "c"); err != nil {
		t.Fatalf("want success, got %v", err)
	}
}

// Conclusion mapping, against real API payloads rather than hand-built states.
//
// An earlier version of this test constructed checkState{successful: true}
// directly and claimed to prove that a skip satisfies protection. It proved
// nothing: it never touched the code that maps a conclusion to a state. Review
// called it theatre, correctly.
func TestMergeCheckRunsMapsConclusions(t *testing.T) {
	body := []byte(`{"check_runs":[
	  {"name":"success-run","status":"completed","conclusion":"success","started_at":"2026-09-13T10:00:00Z"},
	  {"name":"skipped-run","status":"completed","conclusion":"skipped","started_at":"2026-09-13T10:00:00Z"},
	  {"name":"neutral-run","status":"completed","conclusion":"neutral","started_at":"2026-09-13T10:00:00Z"},
	  {"name":"failed-run","status":"completed","conclusion":"failure","started_at":"2026-09-13T10:00:00Z"},
	  {"name":"cancelled-run","status":"completed","conclusion":"cancelled","started_at":"2026-09-13T10:00:00Z"},
	  {"name":"running","status":"in_progress","conclusion":null,"started_at":"2026-09-13T10:00:00Z"}
	]}`)
	states := map[string]checkState{}
	n, err := mergeCheckRuns(states, map[string]int64{}, body)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if n != 6 {
		t.Errorf("counted %d runs, want 6", n)
	}
	want := map[string]struct{ done, successful bool }{
		"success-run":   {true, true},
		"skipped-run":   {true, true}, // a skip satisfies branch protection
		"neutral-run":   {true, true},
		"failed-run":    {true, false},
		"cancelled-run": {true, false},
		"running":       {false, false},
	}
	for name, w := range want {
		got, ok := states[name]
		if !ok {
			t.Errorf("%s: missing", name)
			continue
		}
		if got.done != w.done || got.successful != w.successful {
			t.Errorf("%s: done=%v successful=%v, want done=%v successful=%v",
				name, got.done, got.successful, w.done, w.successful)
		}
	}
}

// A re-run produces a second check-run with the same name. The newest attempt
// must win regardless of the order the API lists them in -- otherwise a stale
// conclusion can override the current one, in either direction.
func TestMergeCheckRunsPrefersNewestAttempt(t *testing.T) {
	// Stale failure listed AFTER the fresh success: last-wins would take it.
	body := []byte(`{"check_runs":[
	  {"name":"structural","status":"completed","conclusion":"success","started_at":"2026-09-13T12:00:00Z"},
	  {"name":"structural","status":"completed","conclusion":"failure","started_at":"2026-09-13T09:00:00Z"}
	]}`)
	states := map[string]checkState{}
	if _, err := mergeCheckRuns(states, map[string]int64{}, body); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if !states["structural"].successful {
		t.Error("an older failed attempt overrode the newer success")
	}

	// And the reverse: a fresh failure must not be masked by an older success.
	body = []byte(`{"check_runs":[
	  {"name":"structural","status":"completed","conclusion":"success","started_at":"2026-09-13T09:00:00Z"},
	  {"name":"structural","status":"completed","conclusion":"failure","started_at":"2026-09-13T12:00:00Z"}
	]}`)
	states = map[string]checkState{}
	if _, err := mergeCheckRuns(states, map[string]int64{}, body); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if states["structural"].successful {
		t.Error("an older success masked the newer failure -- the gate would open wrongly")
	}
}

// A legacy commit status fills in a context no check-run claimed, but must not
// overwrite one that a check-run already reported.
func TestMergeLegacyStatusesDoesNotOverrideCheckRuns(t *testing.T) {
	states := map[string]checkState{"structural": {done: false}}
	mergeLegacyStatuses(states, []byte(`{"statuses":[
	  {"context":"structural","state":"success","target_url":"u1"},
	  {"context":"legacy-only","state":"success","target_url":"u2"}
	]}`))
	if states["structural"].done {
		t.Error("a stale legacy status satisfied a check-run that was still pending")
	}
	if !states["legacy-only"].successful {
		t.Error("a legacy-only context was not picked up")
	}
}

// The deadlock that actually happened: the workflow does not trigger on the
// candidate refs, so no check is ever created. Waiting out the full timeout
// would be correct but useless; the error must name the cause.
func TestAwaitChecksNamesTheMissingTriggerDeadlock(t *testing.T) {
	fetch := func() (map[string]checkState, error) {
		return map[string]checkState{}, nil // nothing ever appears
	}
	start := time.Now()
	err := awaitChecks(fetch, "abcdef123456", []string{"structural"},
		time.Hour,        // long overall timeout ...
		time.Millisecond, // ... but the appear-grace is what should fire
		time.Millisecond, "publish-candidate/abcdef123456")
	if !errors.Is(err, ErrGateTimeout) {
		t.Fatalf("want ErrGateTimeout, got %v", err)
	}
	if !strings.Contains(err.Error(), "does not trigger") {
		t.Errorf("error must name the missing trigger, got: %v", err)
	}
	if time.Since(start) > 30*time.Second {
		t.Errorf("should fail fast on the appear-grace, took %s", time.Since(start))
	}
}

// A check that exists but never finishes is a different failure from one that
// never appears, and must not be reported as the trigger deadlock.
func TestAwaitChecksTimesOutOnPerpetuallyPendingCheck(t *testing.T) {
	fetch := func() (map[string]checkState, error) {
		return map[string]checkState{"structural": {done: false}}, nil
	}
	err := awaitChecks(fetch, "abcdef123456", []string{"structural"},
		10*time.Millisecond, time.Millisecond, time.Millisecond, "c")
	if !errors.Is(err, ErrGateTimeout) {
		t.Fatalf("want ErrGateTimeout, got %v", err)
	}
	if strings.Contains(err.Error(), "does not trigger") {
		t.Errorf("a pending check is not the trigger deadlock, got: %v", err)
	}
	if !strings.Contains(err.Error(), "still waiting") {
		t.Errorf("want the pending message, got: %v", err)
	}
}

func TestAwaitChecksPropagatesFetchError(t *testing.T) {
	boom := errors.New("rate limited")
	fetch := func() (map[string]checkState, error) { return nil, boom }
	err := awaitChecks(fetch, "abcdef123456", []string{"structural"},
		time.Minute, time.Minute, time.Millisecond, "c")
	if !errors.Is(err, boom) {
		t.Fatalf("want the fetch error, got %v", err)
	}
}

func TestGateTimeoutReadsEnv(t *testing.T) {
	t.Setenv("IC_PUBLISH_GATE_TIMEOUT", "45")
	if got := gateTimeout(); got != 45*time.Second {
		t.Errorf("got %s, want 45s", got)
	}
	t.Setenv("IC_PUBLISH_GATE_TIMEOUT", "not-a-number")
	if got := gateTimeout(); got != defaultGateTimeout {
		t.Errorf("garbage should fall back to the default, got %s", got)
	}
}

// The bug this replaced: SyncPeerMarketplaces ran add/commit/push with every
// result discarded. A push that could not land -- an unreachable remote, or a
// branch requiring a status check the publisher never satisfied -- left the
// clone committed but unpushed, silently, drifting further behind on every
// publish. A failed push must reach the caller.
func TestSyncPeerMarketplaces_ReportsAFailedPush(t *testing.T) {
	isolateHome(t)
	src := setupMarketplace(t, pluginEntry{Name: "interbrowse", Version: "0.5.2"})
	peer := setupMarketplace(t, pluginEntry{Name: "interbrowse", Version: "0.5.1"})

	// A real git repo, deliberately with no remote: the commit succeeds and the
	// push cannot.
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "t@example.invalid"},
		{"config", "user.name", "t"},
		{"add", "-A"},
		{"commit", "-qm", "seed"},
	} {
		cmd := exec.Command("git", append([]string{"-C", peer}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s: %v", args, out, err)
		}
	}

	t.Setenv("IC_MARKETPLACE_CLONES", peer)
	err := SyncPeerMarketplaces(src, "interbrowse", "0.5.2")
	if err == nil {
		t.Fatal("a push that could not land was reported as success")
	}
	if !strings.Contains(err.Error(), "push") {
		t.Errorf("error should identify the push, got: %v", err)
	}

	// The manifest update itself still happened -- reporting the push failure
	// must not roll back the local write, or the next publish has nothing to
	// retry from.
	got, rerr := ReadMarketplaceVersion(peer, "interbrowse")
	if rerr != nil {
		t.Fatalf("read peer: %v", rerr)
	}
	if got != "0.5.2" {
		t.Errorf("peer manifest at %s, want 0.5.2 written despite the push failure", got)
	}
}

// Review's most serious finding. UpdateMarketplaceVersion writes the manifest
// BEFORE the push is attempted, and the loop skips any clone whose manifest
// already reads the target version. So after one failed push the clone looked
// "in sync" forever and its local commit was never retried -- while the code
// claimed in an error message that the next publish would retry it.
//
// A second sync must still attempt the push.
func TestSyncPeerMarketplaces_RetriesAClonePreviouslyLeftUnpushed(t *testing.T) {
	isolateHome(t)
	src := setupMarketplace(t, pluginEntry{Name: "interbrowse", Version: "0.5.2"})
	peer := setupMarketplace(t, pluginEntry{Name: "interbrowse", Version: "0.5.1"})

	// A git repo with an upstream that cannot be pushed to: clone from a bare
	// repo, then delete the bare repo so the remote is configured but broken.
	bare := t.TempDir() + "/origin.git"
	mustGit(t, "", "init", "--bare", "-q", bare)
	for _, args := range [][]string{
		{"init", "-q"}, {"config", "user.email", "t@example.invalid"},
		{"config", "user.name", "t"}, {"remote", "add", "origin", bare},
		{"add", "-A"}, {"commit", "-qm", "seed"},
		{"push", "-q", "-u", "origin", "HEAD:refs/heads/main"},
	} {
		mustGit(t, peer, args...)
	}
	if err := os.RemoveAll(bare); err != nil {
		t.Fatal(err)
	}

	t.Setenv("IC_MARKETPLACE_CLONES", peer)

	// First sync: manifest updated, commit made, push fails.
	if err := SyncPeerMarketplaces(src, "interbrowse", "0.5.2"); err == nil {
		t.Fatal("first sync: expected the push failure to be reported")
	}
	if !hasUnpushedCommits(peer) {
		t.Fatal("first sync should have left a local commit to retry")
	}

	// Second sync: the manifest already reads 0.5.2, so the old code skipped
	// here and the commit was stranded. It must retry -- and report again.
	if err := SyncPeerMarketplaces(src, "interbrowse", "0.5.2"); err == nil {
		t.Error("second sync silently skipped a clone with an unpushed commit")
	}
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := args
	if dir != "" {
		full = append([]string{"-C", dir}, args...)
	}
	if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %s: %v", args, out, err)
	}
}
