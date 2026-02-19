package runtrack

import "errors"

var (
	ErrAgentNotFound      = errors.New("agent not found")
	ErrArtifactNotFound   = errors.New("artifact not found")
	ErrRunNotFound        = errors.New("run not found")
	ErrDispatchIDConflict = errors.New("agent already has a dispatch_id")
)
