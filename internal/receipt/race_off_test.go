//go:build !race

package receipt

// raceEnabled reports whether the race detector is active for this build.
// See race_on_test.go for the rationale.
const raceEnabled = false
