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

// OpRuling records the delegation-floor decision for one operation — including
// the operations deliberately left unfloored.
//
// Exempt operations are listed rather than omitted because "no floor" is a
// ruling, not an absence. An op missing from this table has never been
// considered; an op here with Floor 0 was considered and exempted, and the
// reason is on the record. `ic autonomy status` prints both, so telling those
// two cases apart does not require reading this file.
//
// Reason travels with the floor deliberately. The alternative — floors here,
// rationale in docs/canon/autonomy.md — is how the hand-written sentence this
// table replaced went stale: the prose and the code had no reason to change
// together.
type OpRuling struct {
	Op    string `json:"op"`
	Floor int    `json:"floor"`
	// Reason states why, in the terms a human refused by it needs.
	Reason string `json:"reason"`
}

// opRulings is the whole record. Keep it sorted by op; TestOpRulingsAreSorted
// enforces that so the generated canon block has a stable diff.
var opRulings = []OpRuling{
	{
		Op:    "bd-push-dolt",
		Floor: PushAutoFloor,
		Reason: "publishes issue state to the shared Dolt remote — the same " +
			"irreversibility as a git push, on a different substrate",
	},
	{
		Op:    "bead-close",
		Floor: 0,
		Reason: "local and reversible (`bd update --status open` undoes it), and " +
			"frequent enough that a floor would tax ordinary work with no " +
			"safety payoff",
	},
	{
		Op:    "git-push-main",
		Floor: PushAutoFloor,
		Reason: "publishes commits that other people and CI act on, and the " +
			"pushing agent cannot retract them",
	},
	{
		Op:    "ic-publish-patch",
		Floor: 0,
		Reason: "publishing already refuses agent-mutated plugins upstream, so a " +
			"floor here would be a second lock on a bolted door — and it would " +
			"change the publish-wave workflow without evidence it needs changing",
	},
}

// MinLevelForAuto returns the delegation level op requires before policy alone
// may authorize it, or 0 when op carries no delegation floor.
//
// A linear scan over a handful of entries costs nothing on the authorization
// path and keeps opRulings a single ordered source of truth rather than a map
// plus a parallel list.
func MinLevelForAuto(op string) int {
	for _, r := range opRulings {
		if r.Op == op {
			return r.Floor
		}
	}
	return 0
}

// Rulings returns every recorded floor decision, floored and exempt alike, in
// sorted order. Callers render it directly; this is the inspectable surface
// that makes a refusal predictable before it happens.
func Rulings() []OpRuling {
	out := make([]OpRuling, len(opRulings))
	copy(out, opRulings)
	return out
}

// FlooredOps returns only the operations that carry a delegation floor. Order
// is not guaranteed; callers that display it should sort.
func FlooredOps() map[string]int {
	out := make(map[string]int, len(opRulings))
	for _, r := range opRulings {
		if r.Floor > 0 {
			out[r.Op] = r.Floor
		}
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
