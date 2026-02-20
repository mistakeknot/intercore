package discovery

import "errors"

// Sentinel errors — callers use errors.Is() to map CLI exit codes.
var (
	ErrNotFound    = errors.New("discovery not found")
	ErrGateBlocked = errors.New("gate blocked: confidence below promotion threshold")
	ErrDuplicate   = errors.New("duplicate discovery: source/source_id already exists")
	ErrLifecycle   = errors.New("invalid lifecycle transition")
)
