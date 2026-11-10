//go:build !linux

package runner

// Preflight refuses on a host without flock, abstract sockets and /proc.
func Preflight(dbPath string, allowLive bool) (PathInfo, error) {
	return PathInfo{}, refuse("the scheduler runner needs Linux")
}

// NetNS is unavailable off Linux.
func NetNS() (uint64, error) { return 0, refuse("the scheduler runner needs Linux") }
