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
