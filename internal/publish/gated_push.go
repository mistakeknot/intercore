package publish

// Pushing to a branch that requires a status check.
//
// THE PROBLEM
//
// interagency-marketplace's main requires the `structural` check, and that
// requirement binds administrators. A fresh commit has no checks, so pushing
// one straight at main is refused:
//
//	remote: error: GH006: Protected branch update failed for refs/heads/main.
//	remote: - Required status check "structural" is expected.
//
// GitHub does NOT require a pull request to get past this. It accepts a direct
// push of a SHA whose required checks have already succeeded. So the publish
// path lands the SHA on a candidate ref, waits for the check to pass THERE, and
// then advances main to that identical SHA.
//
// The SHA must be identical. Rebasing or amending between the two steps yields
// a new SHA with no checks and the push is refused again, which is why nothing
// here touches the working tree.
//
// WHY THIS POLLS THE API INSTEAD OF JUST RETRYING THE PUSH
//
// Retrying `git push` until the server stops refusing would need no API at all,
// and it was the cheaper design. It was rejected deliberately: it cannot tell
// "the check is still running" from "the check failed", so every failure looks
// like a timeout and the operator has to go read the run themselves. On a
// release path, a precise error is worth the dependency.
//
// THE DEADLOCK THIS GUARDS AGAINST
//
// A check can only exist on a SHA some workflow actually ran against. If the
// validating workflow does not trigger on the candidate refs, nothing will ever
// check the candidate, main will never accept it, and the publish hangs until
// it times out. That is not hypothetical -- it is what happened on the first
// attempt (mk-0y69). waitForChecks detects it by name and says so, rather than
// reporting a bare timeout.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ErrGateTimeout is returned when the required checks did not finish in time.
var ErrGateTimeout = errors.New("required checks did not complete in time")

// ErrGateFailed is returned when a required check completed unsuccessfully. The
// candidate SHA never reaches main.
var ErrGateFailed = errors.New("required check failed")

const (
	defaultGateTimeout = 10 * time.Minute
	// How long to wait for a check to even be CREATED before concluding that no
	// workflow watches the candidate refs. Actions normally register a check run
	// within seconds of the push; a couple of minutes is generous.
	defaultAppearGrace = 2 * time.Minute
	pollInterval       = 5 * time.Second
	candidateRefPrefix = "publish-candidate"
)

var githubRemoteRE = regexp.MustCompile(`github\.com[:/]([^/]+)/(.+?)(?:\.git)?/?$`)

// GitPushGated pushes dir's HEAD to its upstream branch, routing through the
// branch's required status checks when it has any.
//
// When the branch requires no checks -- every other repository intercore
// publishes to, and this one whenever the gate is disarmed -- it degrades to a
// plain GitPush. The gate configures itself from the server; there is no list
// of "gated repositories" to keep in sync with reality.
func GitPushGated(dir string) error {
	if os.Getenv("IC_PUBLISH_GATE") == "off" {
		return GitPush(dir)
	}

	owner, repo, err := gitRemoteSlug(dir)
	if err != nil {
		// Not a GitHub remote: nothing here applies.
		return GitPush(dir)
	}
	branch, err := gitCurrentBranch(dir)
	if err != nil {
		return err
	}

	token := githubToken()
	contexts, err := requiredContexts(owner, repo, branch, token)
	if err != nil {
		return err
	}
	if len(contexts) == 0 {
		return GitPush(dir)
	}

	sha, err := GitHeadCommit(dir)
	if err != nil {
		return err
	}
	if token == "" {
		// Do not quietly fall back to an unverified push. The branch says a check
		// must pass; without a token this process cannot observe that, and
		// guessing is exactly the posture this gate exists to end.
		return fmt.Errorf("%s/%s:%s requires checks %v but no GitHub token is available "+
			"(set GITHUB_TOKEN or GH_TOKEN, or IC_PUBLISH_GATE=off to bypass deliberately)",
			owner, repo, branch, contexts)
	}

	candidate := fmt.Sprintf("%s/%s", candidateRefPrefix, sha[:12])

	// Unique per SHA, so two publishes racing cannot land on the same ref. Two
	// publishes of the SAME SHA are the same content and may share it safely.
	if err := gitPushRef(dir, sha, "refs/heads/"+candidate); err != nil {
		return fmt.Errorf("push candidate ref %s: %w", candidate, err)
	}
	// Always clean up, including on failure and on the error paths below. A
	// leaked candidate ref is small but it accumulates one per failed publish.
	defer func() { _ = gitDeleteRemoteRef(dir, candidate) }()

	if err := waitForChecks(owner, repo, sha, contexts, token, gateTimeout(), candidate); err != nil {
		return err
	}

	// The checks passed for this exact SHA, so the branch accepts it directly.
	if err := gitPushRef(dir, sha, "refs/heads/"+branch); err != nil {
		return fmt.Errorf("advance %s to checked SHA %s: %w", branch, sha[:12], err)
	}
	return nil
}

func gateTimeout() time.Duration {
	if v := os.Getenv("IC_PUBLISH_GATE_TIMEOUT"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second
		}
	}
	return defaultGateTimeout
}

func githubToken() string {
	for _, k := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	// gh stores the token outside the environment for interactive use, which is
	// the normal case on a developer machine.
	out, err := exec.Command("gh", "auth", "token").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func gitRemoteSlug(dir string) (owner, repo string, err error) {
	out, err := exec.Command("git", "-C", dir, "remote", "get-url", "origin").Output()
	if err != nil {
		return "", "", fmt.Errorf("git remote get-url origin: %w", err)
	}
	m := githubRemoteRE.FindStringSubmatch(strings.TrimSpace(string(out)))
	if m == nil {
		return "", "", fmt.Errorf("origin is not a github remote: %s", strings.TrimSpace(string(out)))
	}
	return m[1], m[2], nil
}

func gitCurrentBranch(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse --abbrev-ref HEAD: %w", err)
	}
	b := strings.TrimSpace(string(out))
	if b == "" || b == "HEAD" {
		return "", fmt.Errorf("cannot push from a detached HEAD in %s", dir)
	}
	return b, nil
}

func gitPushRef(dir, sha, dstRef string) error {
	cmd := exec.Command("git", "-C", dir, "push", "origin", sha+":"+dstRef)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(stderr.String()), err)
	}
	return nil
}

func gitDeleteRemoteRef(dir, ref string) error {
	return exec.Command("git", "-C", dir, "push", "origin", "--delete", ref).Run()
}

// requiredContexts returns the status checks the branch requires. An
// unprotected branch, or one with no required checks, yields nil.
func requiredContexts(owner, repo, branch, token string) ([]string, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/branches/%s/protection/required_status_checks",
		owner, repo, branch)
	status, body, err := githubGet(url, token)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
	case http.StatusNotFound:
		// No protection, or protection without required checks.
		return nil, nil
	case http.StatusUnauthorized, http.StatusForbidden:
		// Cannot read protection. Treating that as "no checks required" would
		// send an unverified push at a branch that may well require one.
		return nil, fmt.Errorf("cannot read branch protection for %s/%s:%s (HTTP %d); "+
			"the token may lack repo scope", owner, repo, branch, status)
	default:
		return nil, fmt.Errorf("branch protection for %s/%s:%s: HTTP %d", owner, repo, branch, status)
	}
	var payload struct {
		Contexts []string `json:"contexts"`
		Checks   []struct {
			Context string `json:"context"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("parse required_status_checks: %w", err)
	}
	seen := map[string]bool{}
	var out []string
	for _, c := range append(payload.Contexts, contextNames(payload.Checks)...) {
		if c != "" && !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out, nil
}

func contextNames(checks []struct {
	Context string `json:"context"`
}) []string {
	out := make([]string, 0, len(checks))
	for _, c := range checks {
		out = append(out, c.Context)
	}
	return out
}

// checkState is the outcome of one required context on one SHA.
type checkState struct {
	done       bool
	successful bool
	detailsURL string
}

// waitForChecks blocks until every required context has completed successfully,
// one has failed, or the deadline passes.
func waitForChecks(owner, repo, sha string, contexts []string, token string, timeout time.Duration, candidate string) error {
	return awaitChecks(
		func() (map[string]checkState, error) { return checkStates(owner, repo, sha, token) },
		sha, contexts, timeout, defaultAppearGrace, pollInterval, candidate)
}

// awaitChecks is waitForChecks with its clock and its data source handed in, so
// the deadlock and failure paths can be tested without a network or a wait.
func awaitChecks(
	fetch func() (map[string]checkState, error),
	sha string,
	contexts []string,
	timeout, appearGrace, interval time.Duration,
	candidate string,
) error {
	deadline := time.Now().Add(timeout)
	appearBy := time.Now().Add(appearGrace)
	everAppeared := false

	for {
		states, err := fetch()
		if err != nil {
			return err
		}

		pending := make([]string, 0, len(contexts))
		for _, want := range contexts {
			st, ok := states[want]
			if !ok {
				pending = append(pending, want)
				continue
			}
			everAppeared = true
			if !st.done {
				pending = append(pending, want)
				continue
			}
			if !st.successful {
				return fmt.Errorf("%w: %q on %s (%s)", ErrGateFailed, want, sha[:12], st.detailsURL)
			}
		}
		if len(pending) == 0 {
			return nil
		}

		// The deadlock case, called by name. If no required check has even been
		// created after the grace window, the workflow almost certainly does not
		// trigger on the candidate refs -- so no check will ever exist, and
		// waiting out the full timeout would only delay the same failure.
		if !everAppeared && time.Now().After(appearBy) {
			return fmt.Errorf("%w: no required check (%s) was created for %s after %s. "+
				"The validating workflow probably does not trigger on %s/** refs; "+
				"without that trigger the branch can never accept this commit",
				ErrGateTimeout, strings.Join(pending, ", "), candidate,
				appearGrace, candidateRefPrefix)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: still waiting on %s for %s after %s",
				ErrGateTimeout, strings.Join(pending, ", "), sha[:12], timeout)
		}
		time.Sleep(interval)
	}
}

// checkStates collects both check-runs and legacy commit statuses. A required
// context may be satisfied by either, and reading only one of them would hang
// forever on a repository that uses the other.
func checkStates(owner, repo, sha, token string) (map[string]checkState, error) {
	states := map[string]checkState{}

	status, body, err := githubGet(
		fmt.Sprintf("https://api.github.com/repos/%s/%s/commits/%s/check-runs?per_page=100", owner, repo, sha), token)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("check-runs for %s: HTTP %d", sha[:12], status)
	}
	var runs struct {
		CheckRuns []struct {
			Name       string `json:"name"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			DetailsURL string `json:"details_url"`
		} `json:"check_runs"`
	}
	if err := json.Unmarshal(body, &runs); err != nil {
		return nil, fmt.Errorf("parse check-runs: %w", err)
	}
	for _, r := range runs.CheckRuns {
		done := r.Status == "completed"
		states[r.Name] = checkState{
			done: done,
			// A skipped or neutral run satisfies branch protection the same way a
			// success does; only failure/cancelled/timed_out block.
			successful: done && (r.Conclusion == "success" || r.Conclusion == "skipped" || r.Conclusion == "neutral"),
			detailsURL: r.DetailsURL,
		}
	}

	status, body, err = githubGet(
		fmt.Sprintf("https://api.github.com/repos/%s/%s/commits/%s/status?per_page=100", owner, repo, sha), token)
	if err != nil {
		return nil, err
	}
	if status == http.StatusOK {
		var legacy struct {
			Statuses []struct {
				Context   string `json:"context"`
				State     string `json:"state"`
				TargetURL string `json:"target_url"`
			} `json:"statuses"`
		}
		if err := json.Unmarshal(body, &legacy); err == nil {
			for _, s := range legacy.Statuses {
				if _, taken := states[s.Context]; taken {
					continue // a check-run of the same name already spoke
				}
				states[s.Context] = checkState{
					done:       s.State != "pending",
					successful: s.State == "success",
					detailsURL: s.TargetURL,
				}
			}
		}
	}

	return states, nil
}

var githubHTTP = &http.Client{Timeout: 30 * time.Second}

func githubGet(url, token string) (int, []byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "intercore-publish")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := githubHTTP.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	body := make([]byte, 0, 64*1024)
	buf := make([]byte, 32*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		body = append(body, buf[:n]...)
		if rerr != nil {
			break
		}
		if len(body) > 8*1024*1024 {
			break
		}
	}
	return resp.StatusCode, body, nil
}

// isGitRepo reports whether dir is inside a git working copy. Used to tell a
// plain directory holding a manifest from a clone that can actually publish.
func isGitRepo(dir string) bool {
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--git-dir")
	cmd.Stdout, cmd.Stderr = nil, nil
	return cmd.Run() == nil
}
