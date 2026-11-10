//go:build icsched_test && linux

package runner

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mistakeknot/intercore/internal/dispatch"
	"github.com/mistakeknot/intercore/internal/scheduler"
)

// Config-error reasons. A job that fails a claim-time check ends
// `failed`/`config_error` with the message "<reason>: <detail>". No attempt row
// and no dispatch row exist for it (plan section 5.4, claim-time checks).
const (
	reasonBadSpawnOpts = "bad_spawn_opts"
	reasonGoverned     = "governed_unsupported"
	reasonNoExecutor   = "no_executor"
	reasonNoProjectDir = "no_project_dir"
	reasonMissingPrmpt = "missing_prompt"
	reasonNoOutputBase = "no_output_base"
	reasonOutputExists = "output_exists"
)

// plannedLaunch is what the claim-time checks decided a job will run with.
type plannedLaunch struct {
	opts       dispatch.SpawnOptions
	projectDir string
	outputFile string // <OutputBase>.j<job>.a<attempt>
}

// outputFor is the attempt's output path. It is a pure function of the job and
// the attempt number, so a retry never reuses an earlier attempt's files.
func outputFor(base, jobID string, attemptNo int) string {
	return fmt.Sprintf("%s.j%s.a%d", base, jobID, attemptNo)
}

// sidecars are the files an attempt may leave next to its output.
var sidecars = []string{"", ".verdict", ".summary", ".receipt.json"}

// checkJob runs every claim-time check, in order, before anything is claimed.
// A non-empty reason means the job is a config error with that message.
func (e *engine) checkJob(job *scheduler.SpawnJob) (*plannedLaunch, string) {
	var opts dispatch.SpawnOptions
	var decodeErr error
	if s := strings.TrimSpace(job.SpawnOpts); s != "" {
		// Governed rejection comes first and looks at key presence, so a
		// governed job is never reported as a malformed one. A "role" key
		// is governed even when null (SpawnOptions has no such field, so
		// only a governed writer emits it). A "decision" key is governed
		// unless it is null: older clients serialize `"Decision":null`
		// for every plain job.
		// A document that is not a JSON object cannot be searched for
		// governed keys or checked further, so it is rejected at once.
		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(s), &raw); err != nil {
			return nil, fmt.Sprintf("%s: %v", reasonBadSpawnOpts, err)
		}
		keys := make([]string, 0, len(raw))
		for k := range raw {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			lk := strings.ToLower(k)
			isNull := strings.TrimSpace(string(raw[k])) == "null"
			if lk == "role" || (lk == "decision" && !isNull) {
				return nil, fmt.Sprintf("%s: spawn_opts carries %q; governed dispatch is not supported by this runner", reasonGoverned, k)
			}
		}
		// A field of the wrong type is a bad_spawn_opts error, but it ranks
		// last: the executor and path checks come first. Fields that did
		// decode are still filled in for those checks.
		if err := json.Unmarshal([]byte(s), &opts); err != nil {
			decodeErr = err
		}
	}
	if opts.Decision != nil {
		return nil, fmt.Sprintf("%s: spawn_opts carries a governed decision", reasonGoverned)
	}

	sh := e.o.DispatchSH
	if sh == "" || !filepath.IsAbs(sh) {
		return nil, fmt.Sprintf("%s: the runner has no absolute --dispatch-sh (%q)", reasonNoExecutor, sh)
	}
	if st, err := os.Stat(sh); err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0o111 == 0 {
		return nil, fmt.Sprintf("%s: %s is not an executable regular file", reasonNoExecutor, sh)
	}

	dir := job.ProjectDir
	if dir == "" {
		dir = opts.ProjectDir
	}
	if st, err := os.Stat(dir); dir == "" || err != nil || !st.IsDir() {
		return nil, fmt.Sprintf("%s: %q is not an existing directory", reasonNoProjectDir, dir)
	}

	if opts.PromptFile == "" {
		return nil, fmt.Sprintf("%s: spawn_opts has no PromptFile", reasonMissingPrmpt)
	}
	if st, err := os.Stat(opts.PromptFile); err != nil || !st.Mode().IsRegular() {
		return nil, fmt.Sprintf("%s: %s is not a regular file", reasonMissingPrmpt, opts.PromptFile)
	}

	if job.OutputBase == "" || !filepath.IsAbs(job.OutputBase) {
		return nil, fmt.Sprintf("%s: output base %q is not an absolute path", reasonNoOutputBase, job.OutputBase)
	}
	out := outputFor(job.OutputBase, job.ID, job.AttemptSeq+1)
	for _, suffix := range sidecars {
		if _, err := os.Lstat(out + suffix); err == nil {
			return nil, fmt.Sprintf("%s: %s already exists", reasonOutputExists, out+suffix)
		}
	}

	opts.ProjectDir = dir
	opts.OutputFile = out
	opts.DispatchSH = sh
	if decodeErr != nil {
		return nil, fmt.Sprintf("%s: %v", reasonBadSpawnOpts, decodeErr)
	}
	// Validate exactly what the launch will run, so a configuration Spawn
	// would reject is a config error with no attempt, not a counted prestart
	// failure.
	chk := opts
	chk.StrictExecutor = true
	if err := chk.Validate(); err != nil {
		return nil, fmt.Sprintf("%s: %v", reasonBadSpawnOpts, err)
	}
	return &plannedLaunch{opts: opts, projectDir: dir, outputFile: out}, ""
}
