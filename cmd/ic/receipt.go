package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
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
		fmt.Fprintf(os.Stderr, "ic: receipt: usage: ic receipt <emit|verify|keygen> [args]\n")
		return rcExitOpError
	}
	switch args[0] {
	case "emit":
		return cmdReceiptEmit(ctx, args[1:])
	case "verify":
		return cmdReceiptVerify(ctx, args[1:])
	case "keygen":
		return cmdReceiptKeygen(ctx, args[1:])
	default:
		slog.Error("receipt: unknown subcommand", "subcommand", args[0])
		fmt.Fprintf(os.Stderr, "ic: receipt: unknown subcommand %q (want: emit, verify, keygen)\n", args[0])
		return rcExitOpError
	}
}

// defaultEpoch returns the current calendar quarter as a rotation epoch,
// e.g. "2026-q2". Per canon §Key handling, a new key is provisioned per
// quarter; this gives `emit`/`keygen` a sensible default when --epoch is unset.
func defaultEpoch(t time.Time) string {
	q := (int(t.Month())-1)/3 + 1
	return fmt.Sprintf("%d-q%d", t.Year(), q)
}

// cmdReceiptEmit signs and stores a single action receipt. It self-provisions
// the agent's signing key on first use (EnsureFileKey), so the closed loop
// needs no separate setup. Prints the new receipt_id to stdout (so bash
// callers can capture it).
//
//	ic receipt emit --agent=<id> --model=<m> (--content-hash=<hex> | --content=<str>)
//	               [--parent-run=<id>] [--tool-calls=<json>] [--epoch=<e>] [--keys=<dir>]
func cmdReceiptEmit(ctx context.Context, args []string) int {
	f := cli.ParseFlags(args)
	agentID := f.String("agent", "")
	model := f.String("model", "")
	if agentID == "" || model == "" {
		fmt.Fprintf(os.Stderr, "ic: receipt emit: --agent=<id> and --model=<m> are required\n")
		return rcExitOpError
	}

	// content_hash is the SHA-256 of the action's primary output. Accept it
	// precomputed (--content-hash) or hash a raw string (--content).
	contentHash := f.String("content-hash", "")
	if contentHash == "" {
		if c, ok := f.Raw("content"); ok {
			sum := sha256.Sum256([]byte(c))
			contentHash = hex.EncodeToString(sum[:])
		}
	}
	if contentHash == "" {
		fmt.Fprintf(os.Stderr, "ic: receipt emit: one of --content-hash=<hex> or --content=<str> is required\n")
		return rcExitOpError
	}

	keyRoot := f.String("keys", defaultReceiptKeyRoot)
	epoch := f.String("epoch", defaultEpoch(time.Now()))

	var toolCalls []receipt.ToolCall
	if tc, ok := f.Raw("tool-calls"); ok && tc != "" {
		parsed, err := receipt.ParseToolCallsJSON(tc)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ic: receipt emit: invalid --tool-calls JSON: %v\n", err)
			return rcExitOpError
		}
		toolCalls = parsed
	}

	d, err := openDB()
	if err != nil {
		slog.Error("receipt emit: cannot open db", "error", err)
		return rcExitOpError
	}
	defer d.Close()
	store := receipt.NewStore(d.SqlDB())

	keyID, created, err := receipt.EnsureFileKey(keyRoot, agentID, epoch)
	if err != nil {
		slog.Error("receipt emit: key provisioning failed", "agent", agentID, "error", err)
		return rcExitOpError
	}
	if created {
		fmt.Fprintf(os.Stderr, "ic: receipt emit: provisioned new signing key %s\n", keyID)
	}

	now := time.Now()
	r := receipt.Receipt{
		ReceiptID:     receipt.NewID(now),
		Timestamp:     receipt.FormatTimestamp(now),
		AgentID:       agentID,
		Model:         model,
		ToolCalls:     toolCalls,
		ContentHash:   contentHash,
		SchemaVersion: receipt.SchemaVersion,
	}
	if p, ok := f.Raw("parent-run"); ok && p != "" {
		r.ParentRunID = &p
	}

	ks := &receipt.FileKeyStore{Root: keyRoot}
	canon, err := receipt.Sign(&r, ks, now)
	if err != nil {
		slog.Error("receipt emit: sign failed", "error", err)
		return rcExitOpError
	}
	if err := store.Insert(ctx, &r, canon); err != nil {
		slog.Error("receipt emit: insert failed", "error", err)
		return rcExitOpError
	}

	if flagJSON {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
			"receipt_id": r.ReceiptID,
			"key_id":     r.KeyID,
			"agent_id":   r.AgentID,
		})
	} else {
		fmt.Println(r.ReceiptID)
	}
	return rcExitValid
}

// cmdReceiptKeygen provisions (or rotates) a signing key for an agent.
//
//	ic receipt keygen --agent=<id> [--epoch=<e>] [--keys=<dir>] [--force]
func cmdReceiptKeygen(ctx context.Context, args []string) int {
	f := cli.ParseFlags(args)
	agentID := f.String("agent", "")
	if agentID == "" {
		fmt.Fprintf(os.Stderr, "ic: receipt keygen: --agent=<id> is required\n")
		return rcExitOpError
	}
	keyRoot := f.String("keys", defaultReceiptKeyRoot)
	epoch := f.String("epoch", defaultEpoch(time.Now()))
	keyID, err := receipt.GenerateFileKey(keyRoot, agentID, epoch, f.Bool("force"))
	if err != nil {
		slog.Error("receipt keygen failed", "error", err)
		return rcExitOpError
	}
	if flagJSON {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"key_id": keyID})
	} else {
		fmt.Printf("provisioned signing key: %s\n", keyID)
	}
	return rcExitValid
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
	dur, err := parseSinceDuration(f.String("since", ""))
	if err != nil || dur <= 0 {
		fmt.Fprintf(os.Stderr, "ic: receipt verify: --since requires a positive duration, e.g. --since=24h, --since=7d, --since=2w\n")
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
