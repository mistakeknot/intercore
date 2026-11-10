//go:build icsched_test && linux

package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"syscall"

	"github.com/mistakeknot/intercore/internal/scheduler"
)

const maxReceiptBytes = 64 << 10

// readReceipt reads the executor's receipt, `<output>.receipt.json`, for an
// attempt and returns the evidence class, the sha256 of the file and the
// source to store. The receipt is accepted only when it is bound to this
// attempt (its attempt_id is the attempt's dispatch id or its output path);
// anything else is evidence class "none" with the reason as the source.
func readReceipt(a *scheduler.AttemptRow) (class, sha, source string) {
	if a.DispatchID == "" {
		return "none", "", "no_dispatch"
	}
	if a.OutputFile == "" {
		return "none", "", "no_output"
	}
	f, err := os.OpenFile(a.OutputFile+".receipt.json", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "none", "", "no_receipt"
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return "none", "", "bad_receipt"
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxReceiptBytes+1))
	if err != nil || len(raw) > maxReceiptBytes {
		return "none", "", "bad_receipt"
	}
	sum := sha256.Sum256(raw)
	sha = hex.EncodeToString(sum[:])
	var r struct {
		AttemptID    string `json:"attempt_id"`
		FailureClass string `json:"failure_class"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return "none", sha, "bad_receipt"
	}
	if r.AttemptID == "" || (r.AttemptID != a.DispatchID && r.AttemptID != a.OutputFile) {
		return "none", sha, "unbound_receipt"
	}
	return NormalizeEvidenceClass(r.FailureClass), sha, "receipt"
}
