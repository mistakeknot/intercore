package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/mistakeknot/intercore/contracts"
)

func main() {
	// Determine contracts/ output directory relative to this source file.
	// thisFile is contracts/cmd/gen/main.go, contracts dir is 2 levels up.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		fmt.Fprintln(os.Stderr, "generate: cannot determine source file location")
		os.Exit(1)
	}
	contractsDir := filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))

	if err := contracts.GenerateSchemas(contractsDir); err != nil {
		fmt.Fprintf(os.Stderr, "generate: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Generated schemas in %s\n", contractsDir)
}
