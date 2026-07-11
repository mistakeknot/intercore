package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/mistakeknot/intercore/internal/phase"
)

func setupCommandMetadataDB(t *testing.T) {
	t.Helper()
	oldCWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	oldDB, oldTimeout, oldJSON := flagDB, flagTimeout, flagJSON
	root := t.TempDir()
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	flagDB = "intercore.db"
	flagTimeout = time.Second
	flagJSON = false
	t.Cleanup(func() {
		_ = os.Chdir(oldCWD)
		flagDB, flagTimeout, flagJSON = oldDB, oldTimeout, oldJSON
	})
	if rc := cmdInit(context.Background()); rc != 0 {
		t.Fatalf("cmdInit rc = %d", rc)
	}
}

func TestCmdRunCreateAndSetMetadata(t *testing.T) {
	setupCommandMetadataDB(t)
	ctx := context.Background()
	metadata := `{"z":1,"close_gate":{"requirements":["runtime-evidence/v1"],"bead_id":"sylveste-6h7x"}}`
	if rc := cmdRunCreate(ctx, []string{"--goal=test", "--project=.", "--metadata=" + metadata}); rc != 0 {
		t.Fatalf("cmdRunCreate rc = %d", rc)
	}

	d, err := openDB()
	if err != nil {
		t.Fatal(err)
	}
	store := phase.New(d.SqlDB())
	runs, err := store.List(ctx, nil)
	if err != nil {
		d.Close()
		t.Fatal(err)
	}
	if len(runs) != 1 {
		d.Close()
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	id := runs[0].ID
	d.Close()

	if rc := cmdRunSet(ctx, []string{id, `--metadata-merge={"other":{"value":1}}`}); rc != 0 {
		t.Fatalf("cmdRunSet rc = %d", rc)
	}
	d, err = openDB()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	run, err := phase.New(d.SqlDB()).Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"close_gate":{"bead_id":"sylveste-6h7x","requirements":["runtime-evidence/v1"]},"other":{"value":1},"z":1}`
	if run.Metadata == nil || *run.Metadata != want {
		t.Fatalf("metadata = %v, want %s", run.Metadata, want)
	}
}

func TestCmdRunSetRejectsSealedMetadataWithoutPartialSettings(t *testing.T) {
	setupCommandMetadataDB(t)
	ctx := context.Background()
	metadata := `{"close_gate":{"requirements":["runtime-evidence/v1"],"bead_id":"sylveste-6h7x"}}`
	if rc := cmdRunCreate(ctx, []string{"--goal=test", "--project=.", "--metadata=" + metadata}); rc != 0 {
		t.Fatalf("cmdRunCreate rc = %d", rc)
	}
	d, err := openDB()
	if err != nil {
		t.Fatal(err)
	}
	store := phase.New(d.SqlDB())
	runs, err := store.List(ctx, nil)
	if err != nil {
		d.Close()
		t.Fatal(err)
	}
	id := runs[0].ID
	d.Close()

	rc := cmdRunSet(ctx, []string{id, "--complexity=1", `--metadata-merge={"close_gate":{"bead_id":"other"}}`})
	if rc != 3 {
		t.Fatalf("cmdRunSet sealed rc = %d, want 3", rc)
	}
	d, err = openDB()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	run, err := phase.New(d.SqlDB()).Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if run.Complexity != 3 {
		t.Fatalf("complexity = %d, want unchanged 3", run.Complexity)
	}
}
