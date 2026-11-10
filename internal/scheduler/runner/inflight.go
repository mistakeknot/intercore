package runner

import (
	"fmt"
	"sort"
	"sync"
)

// Stage is where an in-flight claim is. The map is the only record of what
// this runner is doing; there are no other sets.
type Stage int

const (
	StageClaiming Stage = iota + 1
	StageLaunching
	StageWaiting
	StageCompleting
)

func (s Stage) String() string {
	switch s {
	case StageClaiming:
		return "claiming"
	case StageLaunching:
		return "launching"
	case StageWaiting:
		return "waiting"
	case StageCompleting:
		return "completing"
	}
	return fmt.Sprintf("stage(%d)", int(s))
}

type entry struct {
	stage Stage
	gen   uint64
}

// InFlight is the single in-flight map: claim token to stage, behind one
// mutex. Every stage change is a compare-and-set on (token, generation), so an
// owner whose entry was re-inserted by the repair pass does nothing further.
type InFlight struct {
	mu   sync.Mutex
	m    map[string]*entry
	next uint64
}

// NewInFlight returns an empty map.
func NewInFlight() *InFlight { return &InFlight{m: map[string]*entry{}} }

// Insert adds an entry before the claim transaction starts. It returns the
// entry's generation, or false when the token is already present.
func (f *InFlight) Insert(token string, stage Stage) (uint64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.insertLocked(token, stage)
}

func (f *InFlight) insertLocked(token string, stage Stage) (uint64, bool) {
	if _, ok := f.m[token]; ok {
		return 0, false
	}
	f.next++
	f.m[token] = &entry{stage: stage, gen: f.next}
	return f.next, true
}

// legal lists the only forward transitions an owner may make. Nothing leaves
// completing except removal by the main loop after finalize commits.
var legal = map[Stage][]Stage{
	StageClaiming:  {StageLaunching, StageCompleting},
	StageLaunching: {StageWaiting, StageCompleting},
	StageWaiting:   {StageCompleting},
}

// Advance moves (token, gen) to stage to. It returns false, changing nothing,
// when the entry is absent, carries another generation, or the move is not a
// legal forward step.
func (f *InFlight) Advance(token string, gen uint64, to Stage) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.m[token]
	if !ok || e.gen != gen {
		return false
	}
	for _, s := range legal[e.stage] {
		if s == to {
			e.stage = to
			return true
		}
	}
	return false
}

// Discard removes an entry whose claim transaction lost: nothing was claimed,
// so nothing is left to finalize. It applies only to stage claiming.
func (f *InFlight) Discard(token string, gen uint64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.m[token]
	if !ok || e.gen != gen || e.stage != StageClaiming {
		return false
	}
	delete(f.m, token)
	return true
}

// Remove drops an entry after the finalize transaction for its token has
// committed. Only an entry in stage completing can be removed.
func (f *InFlight) Remove(token string, gen uint64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.m[token]
	if !ok || e.gen != gen || e.stage != StageCompleting {
		return false
	}
	delete(f.m, token)
	return true
}

// Stage reports the stage and generation of a token.
func (f *InFlight) Stage(token string) (Stage, uint64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.m[token]
	if !ok {
		return 0, 0, false
	}
	return e.stage, e.gen, true
}

// Len is the number of entries; it counts toward max_concurrent.
func (f *InFlight) Len() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.m)
}

// Item is a token with its generation.
type Item struct {
	Token string
	Gen   uint64
	Stage Stage
}

// InStage lists the entries in a stage, ordered by token for determinism.
func (f *InFlight) InStage(s Stage) []Item {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Item
	for t, e := range f.m {
		if e.stage == s {
			out = append(out, Item{Token: t, Gen: e.gen, Stage: e.stage})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Token < out[j].Token })
	return out
}

// Drop deletes an entry whatever its stage. It exists only for the tagged
// fault hook that simulates a lost entry.
func (f *InFlight) Drop(token string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.m[token]
	delete(f.m, token)
	return ok
}
