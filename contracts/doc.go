// Package contracts maintains the contract type registry for intercore CLI
// output types. It maps CLI subcommand JSON outputs to Go struct definitions
// and provides schema generation for validation.
//
//go:generate go run ./cmd/gen
package contracts
