---
artifact_type: plan
bead: Sylveste-0lt
stage: design
requirements:
  - Sylveste-0lt: prune must never delete the just-published / installed version
---
# Publish Prune Protection Implementation Plan

**Bead:** Sylveste-0lt
**Goal:** `ic publish`'s stale-version prune must never delete the version it just published (clavain 0.6.278 incident, 2026-07-20: the whole plugin cache dir was removed and the plugin went unloadable).

**Root cause (established by code reading):** in `internal/publish/engine.go` the prune runs AFTER `RefreshCCMarketplace()`, which shells out to `claude plugin marketplace update` — a step that can re-clone the marketplace and rewrite `installed_plugins.json` concurrently. `PruneStaleVersionsAcrossMarketplaces` re-reads that file; when a plugin's record is missing or mid-rewrite, `installedVer` is empty and NOTHING is protected — with `keep=1` ⇒ `toKeep=0`, every version of that plugin is deleted. A missing record currently means "delete everything" when it must mean "touch nothing."

**Fix (three layers, per the bead's prescription):**
1. **Fail-safe skip:** no installed record (or empty version) for a plugin ⇒ skip pruning that plugin entirely.
2. **Explicit protection:** the publish flow passes the just-published `<plugin>@<marketplace>` → version into the prune, so the new version is safe regardless of `installed_plugins.json`'s transient state.
3. **Loud post-publish assertion:** after prune/orphan-clean, `engine.go` stats the installPath recorded in `installed_plugins.json`; if missing, emit a hard `ERROR:` line (not a warning) so the failure is visible at publish time, never at next session load.

Refactor for testability: extract the candidate computation into `pruneCandidates(entries, installed, keep, protect) []string` (pure), with the exported wrapper doing I/O — the existing test file explicitly notes the exported function is untestable without HOME redirection.

---

## Task 1: Testable core + protection + fail-safe skip (`internal/publish/cache.go`)

- New pure function `pruneCandidates(entries map[string][]CacheEntry, installed map[string]string, keep int, protect map[string]string) []CacheEntry` implementing: symlink/orphan/installed/protected versions never candidates; **plugins with no installed version AND no protect entry are skipped wholesale**; newest `keep-1` extra versions kept.
- `PruneStaleVersionsAcrossMarketplaces(keep int, protect map[string]string)` — signature gains `protect`; body becomes ReadInstalled + ListAllCacheEntries + `pruneCandidates` + delete loop.
- Update call sites: `engine.go` passes `map[string]string{plugin.Name + "@interagency-marketplace": targetVersion}`; `cmd/ic/publish.go` (clean path) passes `nil`.

## Task 2: Post-publish assertion (`internal/publish/engine.go`)

After the dangling-symlink prune, re-read installed_plugins.json and stat the recorded installPath for the published plugin; on miss:
`e.out("  ERROR: published cache path missing after prune: %s — plugin will fail to load; run ic publish doctor --fix\n", path)`.

## Task 3: Regression tests (`internal/publish/cache_test.go`)

- `TestPruneCandidates_MissingInstalledRecordSkipsPlugin` — entries for a plugin with NO installed record and no protect ⇒ zero candidates (the 0.6.278 incident shape).
- `TestPruneCandidates_ProtectShieldsJustPublished` — installed map EMPTY (simulating the CC rewrite race), protect carries the new version ⇒ the protected version is never a candidate; siblings are.
- `TestPruneCandidates_RespectsInstalledAndKeep` — port of the existing fixture through the new pure core.

## Task 4 (ORCHESTRATOR): rebuild `ic`, real publish, verify

Rebuild `~/.local/bin/ic` from the fixed tree, run `ic publish --patch` in `~/projects/Sylveste/os/Clavain` (this releases the headless-gate fix properly), and verify the new version dir exists in the cache with installed_plugins.json pointing at it.

---

## Acceptance Criteria

1. A plugin with no installed record is skipped wholesale by the prune core (the incident's failure shape can no longer delete anything).
   ```check
   cd ~/projects/Sylveste/core/intercore && go test ./internal/publish/ -run TestPruneCandidates_MissingInstalledRecordSkipsPlugin -v 2>&1 | grep -q "^--- PASS" && echo skip-ok
   ```
2. An explicitly protected (just-published) version survives even with an empty installed map.
   ```check
   cd ~/projects/Sylveste/core/intercore && go test ./internal/publish/ -run TestPruneCandidates_ProtectShieldsJustPublished -v 2>&1 | grep -q "^--- PASS" && echo protect-ok
   ```
3. The publish engine passes explicit protection and asserts the published path exists afterward.
   ```check
   cd ~/projects/Sylveste/core/intercore && grep -q 'PruneStaleVersionsAcrossMarketplaces(1, map\[string\]string{' internal/publish/engine.go && grep -q 'ERROR: published cache path missing after prune' internal/publish/engine.go && echo engine-ok
   ```
4. The whole publish package is green.
   ```check
   cd ~/projects/Sylveste/core/intercore && go test ./internal/publish/ 2>&1 | tail -1 | grep -q "^ok" && echo pkg-ok
   ```
5. A real `ic publish --patch` of clavain leaves the new version present in the cache and referenced by installed_plugins.json.
   ```check
   V=$(python3 -c "import json; d=json.load(open('$HOME/.claude/plugins/installed_plugins.json')); e=d['plugins']['clavain@interagency-marketplace'][0]; import os; assert os.path.isdir(e['installPath']), e['installPath']; print(e['version'])") && ls ~/.claude/plugins/cache/interagency-marketplace/clavain/ | grep -q "$V" && echo publish-intact-ok "$V"
   ```
