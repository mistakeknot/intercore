# orchcompat: `ic` routing characterization tests

These tests pin what `ic` prints, returns and writes today for the routing and
dispatch commands that an orchestration-neutral routing layer must keep
byte-identical. They describe current behavior; they do not judge it. A golden
that differs after a code change means the change altered pinned behavior and
must be reviewed, not that the test is wrong.

## Placement

- `orchcompat/` holds black-box tests. `TestMain` builds `ic` once into a temp
  directory (or uses the binary named by `ORCHCOMPAT_IC`) and runs it as a
  subprocess.
- `internal/routing/tables_characterization_test.go` holds the in-package
  golden for the unexported effort table, lab table and `applyEffortFloor`.
- No existing source file, schema or setting is edited.

## Sandbox

Every case runs in a temp tree: fresh `HOME`, `XDG_*`, data dir, working
directory and context files. `PATH` starts with fake `bb`, `gh`, `codex`,
`claude`, `kimi`, `git`, `curl` and `ssh` that log the call and exit 97; a case
fails if any fake is called. File effects (files created or modified under the
sandbox) are asserted per case. Nothing reaches a network, a model service, a
real database or real thread storage, and no port is opened.

## Fixture

`testdata/routing.yaml` is a routing policy with the shape of the
orchestration plugin's baseline `config/routing.yaml` at its repository commit
`54d2fa43b0022193e502fc4b4e7ede41f04c6e50` (source sha256
`a52fa61626006a71f8a8cfe28666eaa959f31724732484df72be8335012ce2a8`).
It keeps the `subagents`, `reasoning`, `dispatch`, `complexity` and
`calibration` sections. Removed or changed: the per-project sections of
`reasoning` (`projects`, `project_profiles`, `project_aliases`), the pilot
profile's scope value (now `pilot-campaign`), free-text mentions of people and
decision references in `description` fields, and a product-name suffix in two tier
names (now `-alt`). Roles, tiers, aliases, floors, exemptions, profiles and
fallbacks are unchanged. Fixture sha256:
`76fffa83d5b35be3c8030ba75a79f61beacb2540851366b81ff149d6a979af4e`
(16 roles, 41 tiers, 5 frontier reasons). `TestFixtureShapeIsPinned` fails if
the role, reason or tier set changes.

## Running

    go test ./orchcompat ./internal/routing

Goldens live in `orchcompat/testdata/golden/` and
`internal/routing/testdata/`. To regenerate after an intended change:

    go test ./orchcompat -update
    ROUTING_CHARACTERIZATION_UPDATE=1 go test ./internal/routing -run TestTablesCharacterization

Review the golden diff before committing it. Set `ORCHCOMPAT_IC=/path/to/ic` to
run the same suite against another build.

## Coverage

| Area | Test | Golden | Pinned |
| --- | --- | --- | --- |
| Effort table, lab table, effort order selection, `applyEffortFloor` over profiles and reason sets | `TestTablesCharacterization` | `internal/routing/testdata/tables_characterization.golden` | every table entry as exposed by the code |
| `route dispatch --role` for each of 16 fixture roles x (no reason + 5 reasons); review roles also with each producer identity | `TestDispatchRoleMatrix` | `dispatch-role-<role>` | stdout JSON, exit code, stderr, file effects |
| Same roles, text output | `TestDispatchRoleMatrixTextOutput` | `dispatch-role-text` | text form |
| Unsupported role, missing or blank role, missing policy, bad context file, blank rationale, unknown reason, producer errors | `TestDispatchRoleErrors` | `dispatch-errors` | exit codes 1, 2 and 3 with messages |
| Context shapes (handoff states, domain and scope, available-model lists) | `TestDispatchContextShapes` | `dispatch-context-shapes` | outputs and errors |
| `--effort-override` (each level, invalid, uppercase, empty, no role, under floors, claude seats, with producer) | `TestDispatchEffortOverride` | `dispatch-effort-override` | outputs and errors |
| Effort floors on fixture roles for each reason | `TestDispatchEffortFloors` | `dispatch-effort-floors` | raised efforts and floor records |
| Effort floors per backend and model on a synthetic policy (codex, claude, kimi, mismatched backend, unknown backend, bogus effort, bogus floor level) | `TestDispatchEffortFloorsPerBackend` | `dispatch-effort-backends-*` | raises, `unsupported_adapter` errors, policy rejection |
| `--policy-profile` (default, unknown, pilot with and without scope, mapped and unmapped roles, elevated reasons) | `TestDispatchPolicyProfiles` | `dispatch-policy-profiles` | selected seats and errors |
| `--producer-identity` on review roles (concrete ids, aliases) | `TestDispatchProducerIdentities` | `dispatch-producer-identities` | exclusions and fallbacks |
| `--tier` for every fixture tier, `--type` legacy forms | `TestDispatchLegacyTierAndType` | `dispatch-legacy-tier-type` | model output and errors |
| `--calibration` on `route dispatch` with a missing artifact | `TestDispatchCalibrationFlag` | `dispatch-calibration-flag` | output and errors |
| Broken policy files (empty, not YAML, no roles, bad frontier model, bad floor reason, role naming a missing tier, directory, missing) | `TestDispatchPolicyFiles` | `dispatch-policy-files` | errors and exit codes; also `route model/batch/table/identity`, `--tier`, `--type` on a missing policy |
| Policy path precedence: `--policy`, `CLAVAIN_ROUTING_POLICY`, `CLAVAIN_ROOT`, `CLAUDE_PLUGIN_ROOT` (used only if the file exists), home skill link, working-directory walk-up (three locations, nearest ancestor), dangling link, directory | `TestPolicyResolutionPrecedence` | `policy-resolution` | selected `policy_source` and errors |
| `route model` over phase x category x agent | `TestRouteModelMatrix` | `route-model-matrix` | resolved model |
| `route model` and `route batch` with `--calibration` (missing, malformed, empty, array, blank) | `TestRouteModelCalibration` | `route-model-calibration` | output with calibration metadata |
| `route batch` | `TestRouteBatch` | `route-batch` | JSON, text, errors |
| `route table` (sorted before comparison; the command prints in map order) | `TestRouteTable` | `route-table` | rows and floors |
| `route identity` (aliases, prefixes, `[1m]`, dated snapshots, unknown) | `TestRouteIdentity` | `route-identity` | identity or error |
| Unknown or missing `route` subcommand, `dispatch` with no selector | `TestRouteUnknownSubcommand` | `route-unknown` | usage and exit codes |
| `dispatch spawn --role` error order | `TestDispatchSpawnRoleErrors` | `dispatch-spawn-role-errors` | exit codes and messages |
| `dispatch spawn --role --scheduled`: job id output, stored decision, `scheduler status` | `TestDispatchSpawnRoleScheduled` | `dispatch-spawn-role-scheduled` | output, file effects, stored `spawn_opts` read back from the temp database |

## Not covered

| Item | Reason |
| --- | --- |
| `dispatch spawn --role` without `--scheduled` | starts a real backend process; the fakes only prove it is not reached |
| `dispatch retry --escalate` | needs a seeded dispatch row; an in-package test already covers the resolution |
| `route decide`, `record`, `list`, `record-evidence` | registry or database backed, not on the routing policy path |
| Context read from stdin | `--context-file=-` reads a file literally named `-`, so there is no stdin form |
| Real `bb`, `gh`, model services | out of bounds; fakes only |
| Wall-clock values and generated ids | normalized (time field, job ids, backup suffix, creation epoch) |

## Plan readings

- Routing policy source is the sanitized copy of the baseline described above;
  the unsanitized file stays out of the repository.
- The `ic` subcommands found beyond `route dispatch` are `route model`,
  `route batch`, `route table`, `route identity`, `dispatch spawn --role` and
  `scheduler status`; they are covered above. `dispatch retry --escalate` and
  the `route decide`, `record`, `list` and `record-evidence` group are listed
  as not covered.
- Review-role cases run once without a producer identity and once for each of
  two producer identities, because those roles refuse to run without one.
