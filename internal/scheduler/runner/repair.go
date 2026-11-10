package runner

import "fmt"

// RepairRow is the part of an attempt row the repair pass reads.
type RepairRow struct {
	Token        string
	InFlight     bool // the row still carries the token and is in flight
	LaunchResult string
	Exited       bool // exited_at is set
}

// CompletionReady is the only completion predicate. launch_result 'started'
// alone is never completion-ready: a healthy long-running job has it.
func (r RepairRow) CompletionReady() bool {
	if r.Exited {
		return true
	}
	switch r.LaunchResult {
	case "rejected", "hook_aborted", "spawn_error":
		return true
	}
	return false
}

// RepairDeps are the repair pass's reads and its fail-stop. The real fail-stop
// logs the token and stage and exits 5 through exit_group without any DB or
// cgroup write; a unit test stubs it.
type RepairDeps struct {
	// Rows lists the current-epoch in-flight rows.
	Rows func() ([]RepairRow, error)
	// Reread reads one row by claim token. It is called with the map mutex
	// held, so a row finalized after Rows no longer carries its token.
	Reread func(token string) (RepairRow, error)
	// FailStop is called for an absent entry whose row is not completion-ready.
	FailStop func(token, stage string, row RepairRow)
}

// Repair cross-checks the map against the DB. It never changes a present
// entry. For each row with no entry it re-reads the row under the mutex by
// token, skips a row that is gone or finalized, re-inserts a completion-ready
// row as completing under a new generation, and otherwise fail-stops. It
// returns the number of entries it re-inserted.
func (f *InFlight) Repair(d RepairDeps) (int, error) {
	rows, err := d.Rows()
	if err != nil {
		return 0, fmt.Errorf("repair: list rows: %w", err)
	}
	n := 0
	for _, r := range rows {
		reinserted, err := f.repairOne(d, r.Token)
		if err != nil {
			return n, err
		}
		if reinserted {
			n++
		}
	}
	return n, nil
}

func (f *InFlight) repairOne(d RepairDeps, token string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, present := f.m[token]; present {
		return false, nil
	}
	cur, err := d.Reread(token)
	if err != nil {
		return false, fmt.Errorf("repair: re-read %s: %w", token, err)
	}
	if !cur.InFlight {
		return false, nil
	}
	if cur.CompletionReady() {
		f.insertLocked(token, StageCompleting)
		return true, nil
	}
	d.FailStop(token, "absent", cur)
	return false, nil
}
