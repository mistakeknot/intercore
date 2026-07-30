package autonomy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// PushAutoFloor is the lowest delegation level at which a push to a remote may
// proceed without a human in the loop.
//
// It is 3 because that is what the rungs mean. L0-L2 all describe a human who
// is still party to individual actions — approving each one (L0), approving at
// phase gates (L1), or reviewing the evidence afterward (L2). L3 is the first
// rung where the human's contribution is a *policy* rather than a per-action
// decision: "human sets policy, agent executes". A push that proceeds because a
// policy rule's `requires` were satisfied is precisely L3 behavior, so it needs
// L3 to have been declared.
//
// Below L3 the policy is not ignored — a rule that says `block` still blocks.
// The level is a ceiling on how permissive policy is allowed to be, never a
// floor that loosens it.
const PushAutoFloor = 3

// opAutoFloor maps an operation to the delegation level it needs before policy
// alone may authorize it.
//
// Only pushes to a remote are listed. `ic-publish-patch` is deliberately absent:
// publishing already refuses agent-mutated plugins upstream, so gating it here
// would be a second lock on a door that is bolted, and it would change the
// publish-wave workflow without evidence that it needs changing. Adding it is
// one line if that judgement turns out to be wrong.
//
// An operation with no entry has no delegation floor and is governed by policy
// alone, exactly as it was before this table existed.
var opAutoFloor = map[string]int{
	"git-push-main": PushAutoFloor,
	"bd-push-dolt":  PushAutoFloor,
}

// MinLevelForAuto returns the delegation level op requires before policy alone
// may authorize it, or 0 when op carries no delegation floor.
func MinLevelForAuto(op string) int {
	return opAutoFloor[op]
}

// FlooredOps returns the operations that carry a delegation floor. Order is not
// guaranteed; callers that display it should sort.
func FlooredOps() map[string]int {
	out := make(map[string]int, len(opAutoFloor))
	for op, level := range opAutoFloor {
		out[op] = level
	}
	return out
}

// PermitsAuto reports whether a resolved level clears op's delegation floor.
func (r Resolution) PermitsAuto(op string) bool {
	return r.Level >= MinLevelForAuto(op)
}

// ExplainRefusal states why op may not auto-proceed at this level, in terms a
// human reading a refused push needs: which level is in force, whether anyone
// actually declared it, and which level the operation wanted.
func (r Resolution) ExplainRefusal(op string) string {
	floor := MinLevelForAuto(op)
	declared := fmt.Sprintf("declared delegation level L%d (%s)", r.Level, r.Name)
	if !r.Declared {
		declared = fmt.Sprintf("undeclared delegation level, defaulting to L%d (%s)", r.Level, r.Name)
	}
	return fmt.Sprintf("%s is below L%d, which %s requires to proceed without confirmation", declared, floor, op)
}

// ─── Reading the level straight from a database handle ────────────────

// dbGetter reads kernel state from a *sql.DB.
//
// This is the single reader for the delegation level. Both `ic` and callers in
// other modules (clavain-cli, which cannot import intercore's internal
// packages) go through it, so the expiry semantics below exist in one place
// rather than being restated per consumer.
type dbGetter struct{ db *sql.DB }

func (g dbGetter) Get(ctx context.Context, key, scopeID string) (json.RawMessage, error) {
	var payload string
	err := g.db.QueryRowContext(ctx,
		`SELECT payload FROM state
		 WHERE key = ? AND scope_id = ?
		   AND (expires_at IS NULL OR expires_at > unixepoch())`,
		key, scopeID).Scan(&payload)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("autonomy: read %s: %w", key, err)
	}
	return json.RawMessage(payload), nil
}

// NewDBGetter adapts a database handle to the Getter interface.
func NewDBGetter(db *sql.DB) Getter {
	if db == nil {
		return nil
	}
	return dbGetter{db: db}
}

// ResolveDB reads the declared level directly from a database handle.
//
// Like Resolve, it never returns an error: a missing table, a missing row and a
// malformed value all resolve to the visible fallback. Callers on the
// authorization path depend on that — a level that cannot be read must still
// produce a decision, and DefaultLevel (L2) sits below PushAutoFloor, so an
// unreadable level makes pushes confirm rather than sail through.
func ResolveDB(ctx context.Context, db *sql.DB) Resolution {
	return Resolve(ctx, NewDBGetter(db))
}
