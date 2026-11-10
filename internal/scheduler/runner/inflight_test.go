package runner

import (
	"errors"
	"sync"
	"testing"
)

// Unit test 15 (map half): the entry leaves only from completing, and every
// outcome reaches completing by an Advance on (token, generation).
func TestInFlightLifecycle(t *testing.T) {
	f := NewInFlight()
	gen, ok := f.Insert("t1", StageClaiming)
	if !ok {
		t.Fatal("insert")
	}
	if _, ok := f.Insert("t1", StageClaiming); ok {
		t.Fatal("duplicate insert accepted")
	}
	if f.Remove("t1", gen) {
		t.Fatal("removed an entry that never reached completing")
	}
	if !f.Advance("t1", gen, StageLaunching) || !f.Advance("t1", gen, StageWaiting) {
		t.Fatal("legal advance refused")
	}
	if f.Advance("t1", gen, StageLaunching) {
		t.Fatal("went backwards")
	}
	if f.Remove("t1", gen) {
		t.Fatal("removed from waiting")
	}
	if !f.Advance("t1", gen, StageCompleting) {
		t.Fatal("waiting to completing refused")
	}
	if f.Advance("t1", gen, StageWaiting) {
		t.Fatal("left completing")
	}
	if f.Remove("t1", gen+1) {
		t.Fatal("removed with the wrong generation")
	}
	if f.Len() != 1 {
		t.Fatal("a failed finalize must leave the entry")
	}
	if !f.Remove("t1", gen) || f.Len() != 0 {
		t.Fatal("remove after finalize")
	}
}

func TestInFlightOutcomePathsAllReachCompleting(t *testing.T) {
	for name, path := range map[string][]Stage{
		"rejection":                        {StageLaunching, StageCompleting},
		"hook abort":                       {StageLaunching, StageCompleting},
		"spawn err":                        {StageLaunching, StageCompleting},
		"exit":                             {StageLaunching, StageWaiting, StageCompleting},
		"claim win, outcome before launch": {StageCompleting},
	} {
		f := NewInFlight()
		g, _ := f.Insert("t", StageClaiming)
		for _, s := range path {
			if !f.Advance("t", g, s) {
				t.Fatalf("%s: advance to %s refused", name, s)
			}
		}
		if st, _, _ := f.Stage("t"); st != StageCompleting {
			t.Fatalf("%s: ended in %s", name, st)
		}
	}
}

func TestInFlightDiscardOnlyWhileClaiming(t *testing.T) {
	f := NewInFlight()
	g, _ := f.Insert("t", StageClaiming)
	f.Advance("t", g, StageLaunching)
	if f.Discard("t", g) {
		t.Fatal("discarded a launched claim")
	}
	g2, _ := f.Insert("u", StageClaiming)
	if !f.Discard("u", g2) || f.Len() != 1 {
		t.Fatal("discard of a lost claim")
	}
}

func TestInFlightConcurrentAdvanceHasOneWinner(t *testing.T) {
	for i := 0; i < 200; i++ {
		f := NewInFlight()
		g, _ := f.Insert("t", StageLaunching)
		var wg sync.WaitGroup
		wins := make(chan struct{}, 8)
		start := make(chan struct{})
		for j := 0; j < 8; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if f.Advance("t", g, StageCompleting) {
					wins <- struct{}{}
				}
			}()
		}
		close(start)
		wg.Wait()
		if len(wins) != 1 {
			t.Fatalf("%d winners", len(wins))
		}
	}
}

func repairFixture(rows map[string]RepairRow) (RepairDeps, *[]string) {
	var stops []string
	return RepairDeps{
		Rows: func() ([]RepairRow, error) {
			var out []RepairRow
			for _, r := range rows {
				out = append(out, r)
			}
			return out, nil
		},
		Reread: func(tok string) (RepairRow, error) {
			r, ok := rows[tok]
			if !ok {
				return RepairRow{Token: tok}, nil
			}
			return r, nil
		},
		FailStop: func(tok, stage string, _ RepairRow) { stops = append(stops, tok+"/"+stage) },
	}, &stops
}

// Unit test 6c.
func TestRepairPredicate(t *testing.T) {
	t.Run("started alone is never completion-ready", func(t *testing.T) {
		if (RepairRow{InFlight: true, LaunchResult: "started"}).CompletionReady() {
			t.Fatal("ready")
		}
		for _, lr := range []string{"rejected", "hook_aborted", "spawn_error"} {
			if !(RepairRow{InFlight: true, LaunchResult: lr}).CompletionReady() {
				t.Fatalf("%s not ready", lr)
			}
		}
		if !(RepairRow{InFlight: true, LaunchResult: "started", Exited: true}).CompletionReady() {
			t.Fatal("exited not ready")
		}
	})
	t.Run("present entry is never changed", func(t *testing.T) {
		f := NewInFlight()
		g, _ := f.Insert("t", StageWaiting)
		deps, stops := repairFixture(map[string]RepairRow{"t": {Token: "t", InFlight: true, LaunchResult: "spawn_error"}})
		n, err := f.Repair(deps)
		if err != nil || n != 0 || len(*stops) != 0 {
			t.Fatalf("n=%d err=%v stops=%v", n, err, *stops)
		}
		if st, gg, _ := f.Stage("t"); st != StageWaiting || gg != g {
			t.Fatal("entry changed")
		}
	})
	t.Run("finalized row is skipped", func(t *testing.T) {
		f := NewInFlight()
		deps, stops := repairFixture(map[string]RepairRow{"t": {Token: "t", InFlight: true}})
		deps.Reread = func(tok string) (RepairRow, error) { return RepairRow{Token: tok, InFlight: false}, nil }
		if n, _ := f.Repair(deps); n != 0 || len(*stops) != 0 || f.Len() != 0 {
			t.Fatal("acted on a finalized row")
		}
	})
	t.Run("completion-ready absent row is re-inserted under a new generation", func(t *testing.T) {
		f := NewInFlight()
		old, _ := f.Insert("t", StageWaiting)
		if !f.Drop("t") {
			t.Fatal("drop")
		}
		deps, stops := repairFixture(map[string]RepairRow{"t": {Token: "t", InFlight: true, Exited: true, LaunchResult: "started"}})
		n, err := f.Repair(deps)
		if err != nil || n != 1 || len(*stops) != 0 {
			t.Fatalf("n=%d err=%v stops=%v", n, err, *stops)
		}
		st, gen, _ := f.Stage("t")
		if st != StageCompleting || gen == old {
			t.Fatalf("stage %s gen %d old %d", st, gen, old)
		}
		if f.Advance("t", old, StageCompleting) || f.Remove("t", old) {
			t.Fatal("the original owner's stale generation changed the entry")
		}
	})
	t.Run("absent row not completion-ready fail-stops and writes nothing", func(t *testing.T) {
		f := NewInFlight()
		deps, stops := repairFixture(map[string]RepairRow{"t": {Token: "t", InFlight: true, LaunchResult: "started"}})
		n, err := f.Repair(deps)
		if err != nil || n != 0 || len(*stops) != 1 || (*stops)[0] != "t/absent" || f.Len() != 0 {
			t.Fatalf("n=%d err=%v stops=%v len=%d", n, err, *stops, f.Len())
		}
	})
	t.Run("reread error is returned", func(t *testing.T) {
		f := NewInFlight()
		deps, _ := repairFixture(map[string]RepairRow{"t": {Token: "t", InFlight: true}})
		deps.Reread = func(string) (RepairRow, error) { return RepairRow{}, errors.New("busy") }
		if _, err := f.Repair(deps); err == nil {
			t.Fatal("swallowed the error")
		}
	})
}
