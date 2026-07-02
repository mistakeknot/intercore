//go:build race

package receipt

// raceEnabled reports whether the race detector is active for this build.
// Wall-clock perf assertions (TestBulkVerifyPerf) are invalid under -race,
// which slows execution ~10x, so they self-skip when this is true.
const raceEnabled = true
