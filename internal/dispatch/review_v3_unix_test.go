//go:build !windows

package dispatch

import (
	"context"
	"encoding/json"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestReviewV3RecycledPIDCollectsReceipt(t *testing.T) {
	for _, operation := range []string{"kill", "poll", "wait"} {
		t.Run(operation, func(t *testing.T) {
			cmd := exec.Command("sleep", "60")
			cmd.SysProcAttr = platformSysProcAttr()
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			go cmd.Wait()
			t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
			store := testStore(t)
			id, output, receipt := workerReceiptFixture(t, store)
			saveWorkerReceipt(t, output, receipt)
			if err := store.UpdateStatus(context.Background(), id, StatusRunning, UpdateFields{"pid": cmd.Process.Pid}); err != nil {
				t.Fatal(err)
			}
			prior, err := readProcessIdentity(cmd.Process.Pid)
			if err != nil {
				t.Fatal(err)
			}
			prior.Birth += ":prior-attempt"
			payload, _ := json.Marshal(prior)
			if _, err := store.db.Exec(`INSERT INTO state(key,scope_id,payload) VALUES ('dispatch.process',?,?)`, id, string(payload)); err != nil {
				t.Fatal(err)
			}
			switch operation {
			case "kill":
				err = Kill(context.Background(), store, id)
			case "poll":
				_, err = Poll(context.Background(), store, id)
			case "wait":
				_, err = Wait(context.Background(), store, id, time.Millisecond, 20*time.Millisecond)
			}
			if err != nil {
				t.Fatal(err)
			}
			if !isProcessAlive(cmd.Process.Pid) {
				t.Fatal("unrelated recycled process was signalled")
			}
			d, err := store.Get(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if d.Status != StatusCompleted || d.InputTokens != 13 {
				t.Fatalf("collectable receipt discarded: %+v", d)
			}
		})
	}
}
