//go:build nopin

package dispatch

// pinEnabled is false in the nopin build, a negative control for the tests of
// the pin. Never build a binary with this tag.
const pinEnabled = false
