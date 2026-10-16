# ic output goldens

Each file here is the real output of one `ic` command that another repository
reads. `index.json` lists every golden with the exact arguments that produced it
and its format (`json` or `text`). The goldens are the output contract: when a
reader and a golden disagree, the reader is wrong (bead mk-ck9ig).

`TestJSONOutputGoldens` in `cmd/ic/golden_contract_test.go` runs the real `ic`
entry point against a seeded temporary project and compares its output with
these files. It fails when ic's output changes, when a golden is missing, and
when a golden has no command that produces it. It does not stub any ic command.

## Changing ic output

If a change to ic's output is intended:

```bash
go test ./cmd/ic -run TestJSONOutputGoldens -update-goldens
git diff contracts/golden
```

Review the diff as an API change. A removed, renamed or retyped key breaks
readers; list the affected readers (see below) in the pull request and file
work for each one. A new key is safe for readers that ignore unknown keys.

## What is normalized

Values that change from run to run are replaced so the files are stable. Types
are kept, so a timestamp is still a number.

| In the golden | Stands for |
|---------------|------------|
| `RUN_ID`, `BARE_RUN_ID`, `AGENT_ID`, `ARTIFACT_ID`, `GOAL_ID`, `LANE_ID`, `DISPATCH_ID` | the generated ID |
| `/PROJECT` | the project directory |
| `1700000000` | any `*_at` or `*At` timestamp (Unix seconds) |
| `2000-01-01` | any `YYYY-MM-DD` date |
| `0` in `span_id` | a nanosecond clock value |
| `POLICY`, `COST_SCRIPT` in `index.json` | fixture files the test writes |

JSON goldens are pretty-printed with sorted keys. ic itself may print compact
JSON or another key order; neither is part of the contract. Text goldens are
byte-for-byte.

## Shapes readers have got wrong

- `run phase` prints the phase as plain text, even with `--json`.
- Timestamps (`created_at`, `updated_at`, `landed_at`, ...) are Unix seconds as
  numbers, never ISO strings.
- `run agent list` and `dispatch status|list` use `agent_type`, not `type`.
- `run budget` uses `budget` and `used`. With no budget set it prints
  `"budget": null` and a `message`.
- `run events` uses `event_type`, `from_phase`, `to_phase`, a numeric `id` and
  an `envelope` object.
- `cost baseline` reports `landed_changes` and `source`.
- `goal list` and `goal show` use Go field names (`ID`, `Title`, `CreatedAt`),
  and `goal list` prints `null` when there are no goals.
- `lane status` reports `metadata` as a JSON-encoded string, not an object.
- `route model` emits `model`, `phase`, `category` and `agent` (plus
  `calibration` when asked for); there is no `runtime` key.
- `run create --json` emits `id` and `phase`.

## Consuming the goldens from a reader repository

1. Copy the goldens your code reads into the reader repository, for example
   under `testdata/ic-golden/`, together with a `SOURCE` file that records the
   intercore commit they came from:

   ```bash
   sha=$(git -C "$INTERCORE" rev-parse HEAD)
   for f in run-events.json run-budget.json; do
     git -C "$INTERCORE" show "$sha:contracts/golden/$f" > "testdata/ic-golden/$f"
   done
   echo "$sha" > testdata/ic-golden/SOURCE
   ```

2. Test each reader against the golden, not against a hand-written stub. The
   test should decode the golden with the reader's own code and check that
   every field the reader uses came through. A Go reader can check presence
   directly:

   ```go
   var raw []map[string]any
   data, _ := os.ReadFile("testdata/ic-golden/run-agent-list.json")
   json.Unmarshal(data, &raw)
   for _, key := range []string{"id", "agent_type", "created_at"} {
       if _, ok := raw[0][key]; !ok {
           t.Fatalf("reader uses %q, which ic does not emit", key)
       }
   }
   ```

   A shell reader can feed the golden to its stubbed `ic` (for example in a
   bats test) instead of an invented JSON string, so the parsing code runs on
   real output.

3. To pick up ic changes, copy the files again from a newer intercore commit
   and update `SOURCE`. The diff shows exactly what changed for that reader.

Known readers of these commands when the goldens were added:
Autarch (`pkg/intercore`, `internal/icdata`), Intercom (`go/internal/routing`,
`src/query-handlers.ts`, `rust/intercom-core`), Clavain (`cmd/clavain-cli`,
`hooks/lib-sprint.sh`, `hooks/lib-dispatch.sh`) and Interverse interlab
(`internal/experiment`).

## Adding a command

Add a case to `goldenCases` in `cmd/ic/golden_contract_test.go`, seed whatever
the command needs in `seed`, run the update command above and commit the new
golden with `index.json`. Add only commands that another repository reads.
