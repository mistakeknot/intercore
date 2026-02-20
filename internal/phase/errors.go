package phase

import "errors"

var (
	ErrNotFound      = errors.New("run not found")
	ErrNoTransition  = errors.New("no transition from terminal phase")
	ErrStalePhase    = errors.New("phase changed since read (optimistic concurrency conflict)")
	ErrTerminalRun   = errors.New("run is in a terminal status")
	ErrTerminalPhase   = errors.New("run is already in terminal phase")
	ErrInvalidRollback = errors.New("cannot roll back: target phase is not behind current phase")
)
