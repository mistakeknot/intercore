package publish

import (
	"errors"
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

// A skipped job satisfies branch protection. The `sources` job is skipped on
// candidate refs by design, so if it were ever made required, a skip must count.
func TestAwaitChecksTreatsSkippedAsSatisfied(t *testing.T) {
	fetch := func() (map[string]checkState, error) {
		return map[string]checkState{"sources": {done: true, successful: true}}, nil
	}
	if err := awaitChecks(fetch, "abcdef123456", []string{"sources"},
		time.Minute, time.Minute, time.Millisecond, "c"); err != nil {
		t.Fatalf("skipped should satisfy, got %v", err)
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
