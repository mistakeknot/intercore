package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/mistakeknot/intercore/internal/state"
)

const kernelScope = "global"

// Known config keys with their descriptions and defaults.
var knownConfigKeys = map[string]string{
	"global_max_dispatches": "Maximum active dispatches across all runs (0 = unlimited)",
	"max_spawn_depth":       "Maximum dispatch spawn depth (0 = unlimited)",
}

func cmdConfig(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "ic: config: missing subcommand (set, get, list)\n")
		return 3
	}

	switch args[0] {
	case "set":
		return cmdConfigSet(ctx, args[1:])
	case "get":
		return cmdConfigGet(ctx, args[1:])
	case "list":
		return cmdConfigList(ctx)
	default:
		fmt.Fprintf(os.Stderr, "ic: config: unknown subcommand: %s\n", args[0])
		return 3
	}
}

func cmdConfigSet(ctx context.Context, args []string) int {
	if len(args) < 2 {
		fmt.Fprintf(os.Stderr, "ic: config set: usage: ic config set <key> <value>\n")
		fmt.Fprintf(os.Stderr, "\nKnown keys:\n")
		for k, desc := range knownConfigKeys {
			fmt.Fprintf(os.Stderr, "  %-25s %s\n", k, desc)
		}
		return 3
	}

	key := args[0]
	value := args[1]

	// Validate value is a number for known keys
	if _, ok := knownConfigKeys[key]; ok {
		if _, err := strconv.Atoi(value); err != nil {
			fmt.Fprintf(os.Stderr, "ic: config set: value must be an integer: %s\n", value)
			return 3
		}
	}

	// Store under kernel.* namespace in state table
	stateKey := "kernel." + key

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: config set: %v\n", err)
		return 2
	}
	defer d.Close()

	store := state.New(d.SqlDB())
	payload := json.RawMessage(value)
	if err := store.Set(ctx, stateKey, kernelScope, payload, 0); err != nil {
		fmt.Fprintf(os.Stderr, "ic: config set: %v\n", err)
		return 2
	}

	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(map[string]interface{}{
			"key":   key,
			"value": value,
		})
	} else {
		fmt.Printf("%s = %s\n", key, value)
	}
	return 0
}

func cmdConfigGet(ctx context.Context, args []string) int {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "ic: config get: usage: ic config get <key>\n")
		return 3
	}

	key := args[0]
	stateKey := "kernel." + key

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: config get: %v\n", err)
		return 2
	}
	defer d.Close()

	store := state.New(d.SqlDB())
	payload, err := store.Get(ctx, stateKey, kernelScope)
	if err != nil {
		if err == state.ErrNotFound {
			if flagJSON {
				json.NewEncoder(os.Stdout).Encode(map[string]interface{}{
					"key":   key,
					"value": nil,
				})
			} else {
				fmt.Printf("%s: not set\n", key)
			}
			return 1
		}
		fmt.Fprintf(os.Stderr, "ic: config get: %v\n", err)
		return 2
	}

	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(map[string]interface{}{
			"key":   key,
			"value": json.RawMessage(payload),
		})
	} else {
		fmt.Printf("%s = %s\n", key, string(payload))
	}
	return 0
}

func cmdConfigList(ctx context.Context) int {
	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: config list: %v\n", err)
		return 2
	}
	defer d.Close()

	store := state.New(d.SqlDB())

	if flagJSON {
		out := map[string]interface{}{}
		for key := range knownConfigKeys {
			stateKey := "kernel." + key
			payload, err := store.Get(ctx, stateKey, kernelScope)
			if err == nil {
				out[key] = json.RawMessage(payload)
			}
		}
		json.NewEncoder(os.Stdout).Encode(out)
	} else {
		found := false
		for key, desc := range knownConfigKeys {
			stateKey := "kernel." + key
			payload, err := store.Get(ctx, stateKey, kernelScope)
			if err == nil {
				fmt.Printf("%-25s = %s\n", key, string(payload))
				found = true
			} else {
				_ = desc // available for verbose mode
				if flagVerbose {
					fmt.Printf("%-25s   (not set) — %s\n", key, desc)
				}
			}
		}

		// Also list any non-known _kernel keys
		allKeys := listKernelKeys(ctx, store)
		for _, k := range allKeys {
			if _, isKnown := knownConfigKeys[k]; !isKnown {
				stateKey := "_kernel/" + k
				payload, err := store.Get(ctx, stateKey, kernelScope)
				if err == nil {
					fmt.Printf("%-25s = %s\n", k, string(payload))
					found = true
				}
			}
		}

		if !found && !flagVerbose {
			fmt.Println("no config values set (use --verbose to see available keys)")
		}
	}
	return 0
}

// listKernelKeys returns all state keys with the _kernel/ prefix.
func listKernelKeys(ctx context.Context, store *state.Store) []string {
	// The state store doesn't have a prefix-list method, so we check known keys
	// plus any that might exist. For now, return known keys only.
	var keys []string
	for k := range knownConfigKeys {
		keys = append(keys, k)
	}
	return keys
}

// ReadConfigInt reads a kernel config key as an integer. Returns 0 if not set.
func readConfigInt(ctx context.Context, store *state.Store, key string) int {
	stateKey := "kernel." + key
	payload, err := store.Get(ctx, stateKey, kernelScope)
	if err != nil {
		return 0
	}
	val := strings.TrimSpace(string(payload))
	n, err := strconv.Atoi(val)
	if err != nil {
		return 0
	}
	return n
}
