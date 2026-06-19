package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/mistakeknot/intercore/internal/cli"
	"github.com/mistakeknot/intercore/internal/receipt"
)

// Receipt verify exit codes. 0-4 are the canon §Verification semantics
// verdicts (docs/canon/signed-receipts-v1.md) and are part of the scripted
// contract — CI gates and the routing-calibration loop branch on them. 5 is
// reserved for operational failures that prevent verification entirely (DB
// won't open, bad usage), so an infra problem never masquerades as a
// verification verdict.
const (
	rcExitValid       = 0
	rcExitNotFound    = 1
	rcExitInvalidSig  = 2
	rcExitUnsupported = 3
	rcExitUnknownKey  = 4
	rcExitOpError     = 5
)

// defaultReceiptKeyRoot is the project-local keystore root per canon
// §Key handling. Override with --keys=<dir>.
const defaultReceiptKeyRoot = ".clavain/keys/receipts"

func cmdReceipt(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "ic: receipt: usage: ic receipt verify <receipt_id> | ic receipt verify --since=<dur>\n")
		return rcExitOpError
	}
	switch args[0] {
	case "verify":
		return cmdReceiptVerify(ctx, args[1:])
	default:
		slog.Error("receipt: unknown subcommand", "subcommand", args[0])
		fmt.Fprintf(os.Stderr, "ic: receipt: unknown subcommand %q (want: verify)\n", args[0])
		return rcExitOpError
	}
}

// cmdReceiptVerify verifies a single receipt by ID, or (with --since=<dur>)
// bulk-verifies every receipt newer than the cutoff, emitting a JSONL summary.
//
//	ic receipt verify <receipt_id>            # single; exit code is the verdict
//	ic receipt verify --since=7d              # bulk; JSONL per receipt to stdout
//	ic receipt verify <id> --keys=<dir>       # override keystore root
func cmdReceiptVerify(ctx context.Context, args []string) int {
	f := cli.ParseFlags(args)
	keyRoot := f.String("keys", defaultReceiptKeyRoot)

	d, err := openDB()
	if err != nil {
		slog.Error("receipt verify: cannot open db", "error", err)
		return rcExitOpError
	}
	defer d.Close()

	store := receipt.NewStore(d.SqlDB())
	ks := &receipt.FileKeyStore{Root: keyRoot}

	if f.Has("since") {
		return receiptVerifyBulk(ctx, store, ks, f)
	}

	if len(f.Positionals) == 0 {
		fmt.Fprintf(os.Stderr, "ic: receipt verify: usage: ic receipt verify <receipt_id> | --since=<dur> [--keys=<dir>]\n")
		return rcExitOpError
	}
	id := f.Positionals[0]

	r, _, err := store.Get(ctx, id)
	if errors.Is(err, receipt.ErrNotFound) {
		reportVerify(id, "", "not_found", rcExitNotFound)
		return rcExitNotFound
	}
	if err != nil {
		slog.Error("receipt verify: load failed", "id", id, "error", err)
		return rcExitOpError
	}

	verdict, code := verifyVerdict(r, ks)
	reportVerify(id, r.AgentID, verdict, code)
	return code
}

// receiptVerifyBulk walks every receipt newer than --since=<dur> in
// chronological order and emits one JSON object per receipt to stdout
// (JSONL). The aggregate exit code is 0 only if every receipt verified;
// otherwise rcExitInvalidSig signals "at least one did not verify" — the
// per-receipt verdicts in the JSONL carry the detail. Used by CI gates and
// the routing-calibration loop to detect store tampering.
func receiptVerifyBulk(ctx context.Context, store *receipt.Store, ks receipt.KeyStore, f *cli.Flags) int {
	dur, err := f.Duration("since", 0)
	if err != nil || dur <= 0 {
		fmt.Fprintf(os.Stderr, "ic: receipt verify: --since requires a positive duration, e.g. --since=24h\n")
		return rcExitOpError
	}
	since := time.Now().Add(-dur)

	receipts, err := store.List(ctx, receipt.ListOpts{Since: since})
	if err != nil {
		slog.Error("receipt verify: list failed", "error", err)
		return rcExitOpError
	}

	enc := json.NewEncoder(os.Stdout)
	worst := rcExitValid
	failed := 0
	for _, r := range receipts {
		verdict, code := verifyVerdict(r, ks)
		_ = enc.Encode(map[string]any{
			"receipt_id": r.ReceiptID,
			"agent_id":   r.AgentID,
			"timestamp":  r.Timestamp,
			"verdict":    verdict,
			"exit":       code,
		})
		if code != rcExitValid {
			failed++
			// "Some did not verify" — never silently report all-clear.
			worst = rcExitInvalidSig
		}
	}

	fmt.Fprintf(os.Stderr, "verified %d receipt(s) since %s — %d failed\n",
		len(receipts), since.UTC().Format(time.RFC3339), failed)
	return worst
}

// verifyVerdict maps the result of receipt.Verify to a stable verdict string
// and a canon exit code. The HMAC comparison inside receipt.Verify is
// constant-time (crypto/subtle), satisfying acceptance criterion #4.
func verifyVerdict(r *receipt.Receipt, ks receipt.KeyStore) (string, int) {
	err := receipt.Verify(r, ks)
	switch {
	case err == nil:
		return "valid", rcExitValid
	case errors.Is(err, receipt.ErrInvalidSignature):
		return "invalid_signature", rcExitInvalidSig
	case errors.Is(err, receipt.ErrSchemaUnsupported):
		return "unsupported_schema", rcExitUnsupported
	case errors.Is(err, receipt.ErrKeyNotFound):
		return "unknown_key", rcExitUnknownKey
	default:
		// Unexpected error shape — refuse to claim valid. Default-deny per
		// canon §Verification semantics "Never silently accept".
		return "error", rcExitOpError
	}
}

// reportVerify prints a single-receipt verdict. With --json it emits one JSON
// object; otherwise a human line whose wording makes a non-valid verdict
// unmistakable (criterion #3: distinguish 2/3/4, never silently accept).
func reportVerify(id, agentID, verdict string, code int) {
	if flagJSON {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
			"receipt_id": id,
			"agent_id":   agentID,
			"verdict":    verdict,
			"exit":       code,
		})
		return
	}
	switch code {
	case rcExitValid:
		fmt.Printf("receipt %s — VALID (agent %s)\n", id, agentID)
	case rcExitNotFound:
		fmt.Printf("receipt %s — NOT FOUND\n", id)
	case rcExitInvalidSig:
		fmt.Printf("receipt %s — INVALID SIGNATURE (agent %s) — bytes do not match HMAC; DO NOT TRUST\n", id, agentID)
	case rcExitUnsupported:
		fmt.Printf("receipt %s — UNSUPPORTED SCHEMA (agent %s) — this ic binary cannot verify this schema_version\n", id, agentID)
	case rcExitUnknownKey:
		fmt.Printf("receipt %s — UNKNOWN KEY (agent %s) — signing key not in keystore; verification not possible\n", id, agentID)
	default:
		fmt.Printf("receipt %s — ERROR (agent %s) — could not verify\n", id, agentID)
	}
}
