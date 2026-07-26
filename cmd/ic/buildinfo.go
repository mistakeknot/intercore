package main

import (
	"runtime"
	"runtime/debug"
)

// Build provenance for the ic binary.
//
// # WHY THIS EXISTS
//
// `ic version` printed only the compile-time constant "0.3.5" and the LOCAL
// DATABASE schema version. Two machines could -- and did -- report identical
// output while running binaries built from different commits, and nothing on
// either machine recorded which commit either binary came from. A cross-compiled
// artifact deployed to zklw was indistinguishable from one built a month
// earlier. That is the same failure shape as a check that always exits 0: the
// output is present, confident, and carries no information.
//
// Provenance is resolved from two sources, in order:
//
//  1. -ldflags "-X main.buildCommit=... -X main.buildTime=..." if a build script
//     sets them.
//  2. Go's own VCS stamping (runtime/debug.ReadBuildInfo), which the toolchain
//     embeds automatically for builds inside a git work tree.
//
// The fallback is the load-bearing half. Requiring a build script to remember
// ldflags means the stamp goes missing precisely when someone builds in a hurry
// -- which is when provenance matters most. Go's stamping cannot be forgotten.
//
// Both can be absent (`go build` from outside a repo, or -buildvcs=false). That
// case reports SourceUnknown rather than inventing a value, so a health check
// can treat "unstamped" as a finding instead of silently passing.
var (
	buildCommit = ""
	buildTime   = ""
)

// Where a stamp came from. Reported so an operator can tell "built without VCS
// info" apart from "built from commit X".
const (
	SourceLdflags = "ldflags"
	SourceVCS     = "vcs"
	SourceUnknown = "unknown"
)

// BuildStamp describes the provenance of this binary.
type BuildStamp struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Time    string `json:"build_time"`
	Dirty   bool   `json:"dirty"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
	Source  string `json:"source"`
	Go      string `json:"go"`
}

// ShortCommit returns the first 12 characters of the commit, or "" if unstamped.
// Twelve rather than seven: these are compared across machines by a script, and
// seven collides often enough in a repo this size to be worth avoiding.
func (b BuildStamp) ShortCommit() string {
	if len(b.Commit) > 12 {
		return b.Commit[:12]
	}
	return b.Commit
}

// CurrentBuildStamp resolves this binary's provenance.
func CurrentBuildStamp() BuildStamp {
	b := BuildStamp{
		Version: version,
		OS:      runtime.GOOS,
		Arch:    runtime.GOARCH,
		Go:      runtime.Version(),
		Source:  SourceUnknown,
	}

	if buildCommit != "" {
		b.Commit = buildCommit
		b.Time = buildTime
		b.Source = SourceLdflags
		return b
	}

	info, ok := debug.ReadBuildInfo()
	if !ok {
		return b
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			b.Commit = s.Value
		case "vcs.time":
			b.Time = s.Value
		case "vcs.modified":
			// A dirty build is provenance too: it says the binary does not
			// correspond to any commit, so an equality check against HEAD
			// would be answering the wrong question.
			b.Dirty = s.Value == "true"
		}
	}
	if b.Commit != "" {
		b.Source = SourceVCS
	}
	return b
}
