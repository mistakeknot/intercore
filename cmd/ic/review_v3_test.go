package main

import (
	"context"
	"encoding/json"
	"os/exec"
	"testing"

	"github.com/mistakeknot/intercore/internal/dispatch"
)

func TestReviewV3ConfigRejectsNegativeLimits(t *testing.T) {
	for _, key := range []string{"global_max_dispatches", "max_spawn_depth"} {
		t.Run(key, func(t *testing.T) {
			setupCommandMetadataDB(t)
			for _, value := range []string{"0", "1"} {
				if code := cmdConfigSet(context.Background(), []string{key, value}); code != 0 {
					t.Fatalf("valid value %s exit=%d", value, code)
				}
			}
			if code := cmdConfigSet(context.Background(), []string{key, "-1"}); code != 3 {
				t.Fatalf("negative limit exit=%d want3", code)
			}
			db, err := openDB()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var value string
			if err := db.SqlDB().QueryRow(`SELECT payload FROM state WHERE key=? AND scope_id='global'`, "kernel."+key).Scan(&value); err != nil || value != "1" {
				t.Fatalf("valid stored policy overwritten: %s %v", value, err)
			}
		})
	}
}

func TestReviewV3WaitReturnsTerminalRejection(t *testing.T) {
	setupCommandMetadataDB(t)
	flagJSON = true
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go cmd.Wait()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	db, err := openDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.SqlDB().Exec(`INSERT INTO dispatches(id,project_dir,agent_type,status,pid) VALUES ('attempt','.','flere','running',?)`, cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	out := captureDispatchOutput(t, func() int {
		code := cmdDispatchWait(context.Background(), []string{"attempt", "--timeout=20ms", "--poll=1h"})
		if code != 1 {
			t.Errorf("wait exit=%d want1", code)
		}
		return 0
	})
	var terminal dispatch.DispatchOutput
	if err := json.Unmarshal(out, &terminal); err != nil {
		t.Fatalf("no terminal JSON: %q %v", out, err)
	}
	if terminal.Status != dispatch.StatusFailed {
		t.Fatalf("terminal=%+v", terminal)
	}
}
