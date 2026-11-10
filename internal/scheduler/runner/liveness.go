//go:build icsched_test

package runner

import "time"

// Liveness of the runner a database names (plan section 5.3 item 2).
const (
	LivenessAlive  = "alive"  // running, fresh heartbeat, verified process
	LivenessHung   = "hung"   // verified process, but stale heartbeat or not running
	LivenessAbsent = "absent" // anything else
)

// DefaultStaleAfter is three heartbeat intervals.
const DefaultStaleAfter = 15 * time.Second

// ClassifyLiveness classifies a runner row without taking any lock. procPresent
// is the result of Holder.Present for the row's pid, start time and boot id.
// A state other than running is never alive: a draining or stopped runner takes
// no new work.
func ClassifyLiveness(state string, heartbeatAge, staleAfter time.Duration, procPresent bool) string {
	if !procPresent {
		return LivenessAbsent
	}
	if state != "running" {
		return LivenessAbsent
	}
	if heartbeatAge > staleAfter {
		return LivenessHung
	}
	return LivenessAlive
}
