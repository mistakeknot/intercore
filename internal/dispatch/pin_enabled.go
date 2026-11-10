//go:build !nopin

package dispatch

// pinEnabled keeps one statement open on the admission connection for the
// hook's lifetime (see pinConn). Only the nopin build tag turns it off, so the
// lost-interrupt test can be shown to fail without it.
const pinEnabled = true
