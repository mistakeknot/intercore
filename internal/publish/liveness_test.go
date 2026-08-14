package publish

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The 2026-08-11 incident, reproduced against the real prune.
//
// intermux 0.1.9 was stale by every rule the prune knew: not the installed
// version, not the just-published one, not marketplace-referenced, not
// .orphaned_at-marked, and old enough to fall outside keep-1. It was also the
// working set of 23 running intermux-mcp servers. The prune deleted it; the
// servers ran on from the unlinked inode, and nothing could name their version
// again because the manifest went with the directory.
//
// This drives pruneStaleVersions itself rather than restating its filter, so it
// fails against the unconditional prune it was written to prevent.
func TestPruneStaleVersions_KeepsDirectoryWithRunningProcess(t *testing.T) {
	root := makeCacheTree(t, [][3]string{
		{"interagency-marketplace", "intermux", "0.1.9"},  // held by live servers
		{"interagency-marketplace", "intermux", "0.1.11"}, // stale, nobody running it
		{"interagency-marketplace", "intermux", "0.1.12"}, // installed
	})
	entries, err := listAllCacheEntriesIn(root)
	if err != nil {
		t.Fatalf("listAllCacheEntriesIn: %v", err)
	}
	installed := map[string]string{"intermux@interagency-marketplace": "0.1.12"}

	heldDir := filepath.Join(root, "interagency-marketplace", "intermux", "0.1.9")
	staleDir := filepath.Join(root, "interagency-marketplace", "intermux", "0.1.11")

	// 23 servers, as observed.
	var procs []ProcExe
	for i := 0; i < 23; i++ {
		procs = append(procs, ProcExe{PID: 86338 + i, Exe: filepath.Join(heldDir, "bin", "intermux-mcp")})
	}

	// The old prune deleted exactly what pruneCandidates returned, with no
	// liveness gate — so this fixture only exercises the interlock while policy
	// still considers 0.1.9 stale. Asserted rather than assumed: if a later
	// change to keep-N or the guards quietly protected 0.1.9 for some other
	// reason, the test below would keep passing while guarding nothing.
	policySelected := false
	for _, c := range pruneCandidates(entries, installed, 1, nil) {
		if c.Version == "0.1.9" {
			policySelected = true
		}
	}
	if !policySelected {
		t.Fatal("fixture no longer exercises the interlock: policy does not consider 0.1.9 deletable, " +
			"so keeping it proves nothing")
	}

	report := pruneStaleVersions(root, entries, installed, 1, nil, procs)

	if _, err := os.Stat(heldDir); err != nil {
		t.Fatalf("0.1.9 was deleted while 23 processes were executing it: %v", err)
	}
	if _, err := os.Stat(staleDir); !os.IsNotExist(err) {
		t.Errorf("0.1.11 had no live holder and should have been pruned")
	}
	if report.Pruned != 1 {
		t.Errorf("Pruned = %d, want 1 (only the unheld version)", report.Pruned)
	}
	if len(report.Held) != 1 {
		t.Fatalf("Held = %d entries, want 1", len(report.Held))
	}
	h := report.Held[0]
	if h.Version != "0.1.9" || h.Key != "intermux@interagency-marketplace" {
		t.Errorf("held entry = %s %s, want intermux@interagency-marketplace 0.1.9", h.Key, h.Version)
	}
	if len(h.Holders) != 23 {
		t.Errorf("Holders = %d, want 23", len(h.Holders))
	}
	// The report must name pids: "something is in use" without saying what to
	// restart is not actionable, and this is the only place that mapping exists.
	summary := h.Summary()
	for _, want := range []string{"intermux@interagency-marketplace", "0.1.9", "23 processes", "86338", "+18 more"} {
		if !strings.Contains(summary, want) {
			t.Errorf("Summary() = %q, missing %q", summary, want)
		}
	}
}

// An empty process table means the question went unanswered, not that nothing
// is running. Pruning on that basis is exactly the deletion this file prevents.
func TestPruneStaleVersions_EmptyProcessTableDeclinesEntirely(t *testing.T) {
	root := makeCacheTree(t, [][3]string{
		{"interagency-marketplace", "intermux", "0.1.9"},
		{"interagency-marketplace", "intermux", "0.1.12"},
	})
	entries, err := listAllCacheEntriesIn(root)
	if err != nil {
		t.Fatalf("listAllCacheEntriesIn: %v", err)
	}
	installed := map[string]string{"intermux@interagency-marketplace": "0.1.12"}

	report := pruneStaleVersions(root, entries, installed, 1, nil, nil)

	if report.Pruned != 0 {
		t.Errorf("Pruned = %d, want 0 when the process table is unreadable", report.Pruned)
	}
	if report.Blocked == "" {
		t.Error("Blocked must say why nothing was pruned; silence reads as 'nothing to do'")
	}
	if _, err := os.Stat(filepath.Join(root, "interagency-marketplace", "intermux", "0.1.9")); err != nil {
		t.Errorf("nothing may be deleted when liveness is unknown: %v", err)
	}
}

// With a populated process table and no holders, pruning is unchanged. Without
// this, "protect everything" would pass the two tests above and quietly stop
// the cache from ever being cleaned.
func TestPruneStaleVersions_PrunesWhenNoProcessHoldsIt(t *testing.T) {
	root := makeCacheTree(t, [][3]string{
		{"interagency-marketplace", "intermux", "0.1.9"},
		{"interagency-marketplace", "intermux", "0.1.12"},
	})
	entries, err := listAllCacheEntriesIn(root)
	if err != nil {
		t.Fatalf("listAllCacheEntriesIn: %v", err)
	}
	installed := map[string]string{"intermux@interagency-marketplace": "0.1.12"}

	// A real process table, none of it inside the cache.
	procs := []ProcExe{{PID: 1, Exe: "/sbin/launchd"}, {PID: 42, Exe: "/usr/bin/ssh"}}
	report := pruneStaleVersions(root, entries, installed, 1, nil, procs)

	if report.Pruned != 1 || len(report.Held) != 0 {
		t.Errorf("Pruned = %d, Held = %d; want 1 pruned and nothing held",
			report.Pruned, len(report.Held))
	}
	if _, err := os.Stat(filepath.Join(root, "interagency-marketplace", "intermux", "0.1.9")); !os.IsNotExist(err) {
		t.Error("0.1.9 had no holder and should have been pruned")
	}
}

// The orphan cleaner is the path that actually bit, and it bit while the
// version-prune interlock above was already in place.
//
// Observed on Clavain 2026-08-14, using a build that carried the prune fix:
// `ic publish clean` unlinked intermux 0.1.11 while two servers were executing
// it. The deletion came through the .orphaned_at cleaner, which CleanOrphans
// calls with minAge 0 — no grace window at all — and which never asked what was
// running. A guard on one of two delete paths is not a guard.
func TestCleanOrphans_KeepsMarkedDirectoryWithRunningProcess(t *testing.T) {
	root := makeCacheTree(t, [][3]string{
		{"interagency-marketplace", "intermux", "0.1.11"}, // marked AND running
		{"interagency-marketplace", "tldr-swinton", "0.7.18"},
	})
	heldDir := filepath.Join(root, "interagency-marketplace", "intermux", "0.1.11")
	idleDir := filepath.Join(root, "interagency-marketplace", "tldr-swinton", "0.7.18")
	for _, d := range []string{heldDir, idleDir} {
		if err := os.WriteFile(filepath.Join(d, ".orphaned_at"), []byte("1"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	procs := []ProcExe{
		{PID: 54076, Exe: filepath.Join(heldDir, "bin", "intermux-mcp")},
		{PID: 22658, Exe: filepath.Join(heldDir, "bin", "intermux-mcp")},
	}

	// minAge 0 — the zero-grace path `ic publish clean` takes, where the age
	// heuristic offers no protection whatsoever.
	report, err := cleanOrphansIn(root, 0, procs)
	if err != nil {
		t.Fatalf("cleanOrphansIn: %v", err)
	}

	if _, err := os.Stat(heldDir); err != nil {
		t.Fatalf("0.1.11 was unlinked while 2 processes were executing it: %v", err)
	}
	if _, err := os.Stat(idleDir); !os.IsNotExist(err) {
		t.Error("a marked orphan with no live holder should still be cleaned")
	}
	if report.Pruned != 1 {
		t.Errorf("Pruned = %d, want 1", report.Pruned)
	}
	if len(report.Held) != 1 || report.Held[0].Version != "0.1.11" {
		t.Fatalf("Held = %+v, want one entry for 0.1.11", report.Held)
	}
	if got := report.Held[0].Key; got != "intermux@interagency-marketplace" {
		t.Errorf("Key = %q, want intermux@interagency-marketplace", got)
	}
	if !strings.Contains(report.Held[0].Summary(), "54076") {
		t.Errorf("Summary() = %q, must name the pids to restart", report.Held[0].Summary())
	}
}

// The preview must not promise a deletion the real command will refuse. Shipped
// without this, `--dry-run` counted held directories among the ones it "would
// clean" — which is the wrong answer to the only question a dry-run is asked.
// Fixture is the 2026-08-14 state on Clavain: intermux 0.1.13 installed, 0.1.12
// marked orphaned by `claude plugin update` with six sessions still inside it.
func TestCountStaleIn_ReportsHeldSeparatelyFromDeletable(t *testing.T) {
	root := makeCacheTree(t, [][3]string{
		{"interagency-marketplace", "intermux", "0.1.12"},     // orphaned AND held
		{"interagency-marketplace", "intermux", "0.1.13"},     // installed
		{"interagency-marketplace", "tldr-swinton", "0.7.18"}, // orphaned, nobody home
		{"interagency-marketplace", "tldr-swinton", "0.7.19"}, // installed
		{"claude-plugins-official", "vercel", "0.40.0"},       // stale, nobody home
		{"claude-plugins-official", "vercel", "0.42.1"},       // installed
	})
	heldDir := filepath.Join(root, "interagency-marketplace", "intermux", "0.1.12")
	idleOrphan := filepath.Join(root, "interagency-marketplace", "tldr-swinton", "0.7.18")
	for _, d := range []string{heldDir, idleOrphan} {
		if err := os.WriteFile(filepath.Join(d, ".orphaned_at"), []byte("1"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	entries, err := listAllCacheEntriesIn(root)
	if err != nil {
		t.Fatalf("listAllCacheEntriesIn: %v", err)
	}
	installed := map[string]string{
		"intermux@interagency-marketplace":     "0.1.13",
		"tldr-swinton@interagency-marketplace": "0.7.19",
		"vercel@claude-plugins-official":       "0.42.1",
	}

	var procs []ProcExe
	for _, pid := range []int{23010, 86616, 60460, 1623, 62684, 96059} {
		procs = append(procs, ProcExe{PID: pid, Exe: filepath.Join(heldDir, "bin", "intermux-mcp")})
	}

	orphaned, stale, held, blocked := countStaleIn(root, entries, installed, procs)

	if blocked != "" {
		t.Errorf("blocked = %q, want empty: the process table was readable", blocked)
	}
	if orphaned != 1 {
		t.Errorf("orphaned = %d, want 1 — the held 0.1.12 must NOT be counted as deletable", orphaned)
	}
	if stale != 1 {
		t.Errorf("stale = %d, want 1 (vercel 0.40.0)", stale)
	}
	if len(held) != 1 {
		t.Fatalf("held = %+v, want exactly one entry", held)
	}
	if held[0].Key != "intermux@interagency-marketplace" || held[0].Version != "0.1.12" {
		t.Errorf("held = %s %s, want intermux@interagency-marketplace 0.1.12", held[0].Key, held[0].Version)
	}
	if len(held[0].Holders) != 6 {
		t.Errorf("Holders = %d, want 6", len(held[0].Holders))
	}
	if !strings.Contains(held[0].Summary(), "23010") {
		t.Errorf("Summary() = %q, must name pids to restart", held[0].Summary())
	}
}

// An unreadable process table makes the preview's counts meaningless in the same
// way it makes the real prune unsafe, so the dry-run says so rather than
// printing numbers it cannot stand behind.
func TestCountStaleIn_EmptyProcessTableReportsDeclineUpFront(t *testing.T) {
	root := makeCacheTree(t, [][3]string{
		{"interagency-marketplace", "intermux", "0.1.12"},
		{"interagency-marketplace", "intermux", "0.1.13"},
	})
	entries, err := listAllCacheEntriesIn(root)
	if err != nil {
		t.Fatalf("listAllCacheEntriesIn: %v", err)
	}
	installed := map[string]string{"intermux@interagency-marketplace": "0.1.13"}

	_, _, _, blocked := countStaleIn(root, entries, installed, nil)
	if blocked == "" {
		t.Error("empty process table must produce a decline notice, not a silent count")
	}
}

func TestCleanOrphans_EmptyProcessTableDeclinesEntirely(t *testing.T) {
	root := makeCacheTree(t, [][3]string{{"interagency-marketplace", "intermux", "0.1.11"}})
	dir := filepath.Join(root, "interagency-marketplace", "intermux", "0.1.11")
	if err := os.WriteFile(filepath.Join(dir, ".orphaned_at"), []byte("1"), 0644); err != nil {
		t.Fatal(err)
	}
	report, err := cleanOrphansIn(root, 0, nil)
	if err != nil {
		t.Fatalf("cleanOrphansIn: %v", err)
	}
	if report.Pruned != 0 || report.Blocked == "" {
		t.Errorf("Pruned = %d, Blocked = %q; want 0 and a stated reason", report.Pruned, report.Blocked)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("nothing may be unlinked when liveness is unknown: %v", err)
	}
}

// End-to-end against a REAL running process and the REAL process table.
//
// Every other test here injects a synthetic []ProcExe, which proves the
// decision logic but not that this machine can actually see its own processes.
// That gap is where a cross-platform enumerator fails silently: RunningExecutables
// returning nothing degrades the interlock into "decline everything", which no
// synthetic test would notice. Here a real binary is executed from inside a
// cache tree and must be observed by the same code path publish uses.
func TestCleanOrphans_LiveProcessEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a helper binary")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain to build the helper")
	}

	root := makeCacheTree(t, [][3]string{{"interagency-marketplace", "liveplugin", "9.9.9"}})
	vdir := filepath.Join(root, "interagency-marketplace", "liveplugin", "9.9.9")
	if err := os.MkdirAll(filepath.Join(vdir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(vdir, "bin", "liveplugin-mcp")

	// Built rather than copied from /bin/sleep: macOS SIGKILLs a relocated
	// platform binary (verified — exit 137), so a copy would never run and the
	// test would "pass" by observing nothing. Building also mirrors reality,
	// where a plugin's binary is compiled into its cache dir.
	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "main.go")
	if err := os.WriteFile(src, []byte(
		"package main\nimport \"time\"\nfunc main() { time.Sleep(2 * time.Minute) }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-o", exe, src)
	build.Env = append(os.Environ(), "GOFLAGS=")
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("cannot build helper: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(vdir, ".orphaned_at"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(exe)
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot exec from temp dir (noexec mount?): %v", err)
	}
	killed := false
	stop := func() {
		if !killed {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
			killed = true
		}
	}
	t.Cleanup(stop)

	// Give the kernel a moment to publish the process before reading the table.
	var report PruneReport
	var err error
	for i := 0; i < 50; i++ {
		report, err = cleanOrphansIn(root, 0, RunningExecutables())
		if err != nil {
			t.Fatalf("cleanOrphansIn: %v", err)
		}
		if len(report.Held) > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if _, statErr := os.Stat(vdir); statErr != nil {
		t.Fatalf("a directory with a live process in it was unlinked: %v", statErr)
	}
	if len(report.Held) != 1 {
		t.Fatalf("Held = %+v, want the live version dir", report.Held)
	}
	if report.Held[0].Holders[0].PID != cmd.Process.Pid {
		t.Errorf("holder pid = %d, want %d", report.Held[0].Holders[0].PID, cmd.Process.Pid)
	}

	// And once nothing holds it, the same call cleans it — proving the guard
	// releases rather than protecting the directory forever.
	stop()
	for i := 0; i < 50; i++ {
		report, err = cleanOrphansIn(root, 0, RunningExecutables())
		if err != nil {
			t.Fatalf("cleanOrphansIn after kill: %v", err)
		}
		if report.Pruned > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, statErr := os.Stat(vdir); !os.IsNotExist(statErr) {
		t.Errorf("directory should be cleaned once its holder exits; Held=%+v", report.Held)
	}
}

func TestHeldVersionDirs(t *testing.T) {
	root := t.TempDir()
	vdir := filepath.Join(root, "interagency-marketplace", "intermux", "0.1.9")

	cases := []struct {
		name string
		exe  string
		want bool
	}{
		{"binary inside the version dir", filepath.Join(vdir, "bin", "intermux-mcp"), true},
		{"file directly in the version dir", filepath.Join(vdir, "run"), true},
		{"deeply nested", filepath.Join(vdir, "a", "b", "c", "x"), true},
		{"the version dir itself is too shallow", vdir, false},
		{"plugin dir, not a version", filepath.Join(root, "interagency-marketplace", "intermux", "x"), false},
		{"outside the cache entirely", "/usr/bin/ssh", false},
		{"relative path cannot be attributed", "intermux-mcp", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			held := heldVersionDirs(root, []ProcExe{{PID: 7, Exe: tc.exe}})
			_, got := held[vdir]
			if got != tc.want {
				t.Errorf("held[%s] = %v, want %v (exe %q)", vdir, got, tc.want, tc.exe)
			}
		})
	}
}

// A sibling path that merely shares a prefix must not be mistaken for the
// version dir: "0.1.90" starts with "0.1.9".
func TestHeldVersionDirs_NoPrefixConfusion(t *testing.T) {
	root := t.TempDir()
	held := heldVersionDirs(root, []ProcExe{
		{PID: 1, Exe: filepath.Join(root, "interagency-marketplace", "intermux", "0.1.90", "bin", "x")},
	})
	target := filepath.Join(root, "interagency-marketplace", "intermux", "0.1.9")
	if _, ok := held[target]; ok {
		t.Error("0.1.90 was attributed to 0.1.9")
	}
}

func TestRunningFromProc(t *testing.T) {
	// A fake procfs, so the Linux branch is exercised wherever this suite runs.
	proc := t.TempDir()
	target := filepath.Join(t.TempDir(), "intermux-mcp")
	if err := os.WriteFile(target, []byte("x"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, pid := range []string{"101", "202"} {
		if err := os.MkdirAll(filepath.Join(proc, pid), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(proc, pid, "exe")); err != nil {
			t.Fatal(err)
		}
	}
	// Non-numeric entries and pids without a readable exe must be skipped, not
	// abort the walk.
	if err := os.MkdirAll(filepath.Join(proc, "self"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(proc, "303"), 0755); err != nil {
		t.Fatal(err)
	}

	procs, ok := runningFromProc(proc)
	if !ok {
		t.Fatal("a procfs with numeric entries must report ok")
	}
	if len(procs) != 2 {
		t.Fatalf("got %d processes, want 2: %+v", len(procs), procs)
	}
	for _, p := range procs {
		if p.Exe != target {
			t.Errorf("Exe = %q, want %q", p.Exe, target)
		}
	}
}

func TestRunningFromProc_AbsentProcfsIsNotAnEmptySystem(t *testing.T) {
	// The macOS case. Must report NOT-ok so the caller falls through to ps
	// rather than concluding that nothing is running.
	if _, ok := runningFromProc(filepath.Join(t.TempDir(), "nonexistent")); ok {
		t.Error("a missing procfs must report ok=false")
	}
	// An existing directory with no numeric entries is the same claim.
	if _, ok := runningFromProc(t.TempDir()); ok {
		t.Error("a procfs with no pids must report ok=false")
	}
}

// Linux marks an unlinked binary — the very state this guard prevents — and the
// marker must not become part of the path.
func TestRunningFromProc_StripsDeletedMarker(t *testing.T) {
	proc := t.TempDir()
	if err := os.MkdirAll(filepath.Join(proc, "404"), 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(proc, "404", "exe")
	if err := os.Symlink("/cache/intermux/0.1.9/bin/intermux-mcp (deleted)", link); err != nil {
		t.Fatal(err)
	}
	procs, ok := runningFromProc(proc)
	if !ok || len(procs) != 1 {
		t.Fatalf("ok=%v procs=%+v", ok, procs)
	}
	if procs[0].Exe != "/cache/intermux/0.1.9/bin/intermux-mcp" {
		t.Errorf("Exe = %q, want the marker stripped", procs[0].Exe)
	}
}

func TestParsePS(t *testing.T) {
	procs := parsePS(`  101 /usr/bin/ssh
  202 /Users/x/.claude/plugins/cache/m/intermux/0.1.9/bin/intermux-mcp
  303 /Applications/Some App.app/Contents/MacOS/Some App
garbage line

  xyz /not/a/pid
`)
	if len(procs) != 3 {
		t.Fatalf("got %d, want 3: %+v", len(procs), procs)
	}
	if procs[2].Exe != "/Applications/Some App.app/Contents/MacOS/Some App" {
		t.Errorf("a path containing spaces was truncated: %q", procs[2].Exe)
	}
	if procs[0].PID != 101 {
		t.Errorf("PID = %d, want 101", procs[0].PID)
	}
}

// RunningExecutables must see this very test binary. If it ever returns
// nothing on a live system the interlock silently degrades into "decline
// everything", which is safe but would stop the cache being cleaned forever.
func TestRunningExecutables_SeesItself(t *testing.T) {
	procs := RunningExecutables()
	if len(procs) == 0 {
		t.Fatal("no processes enumerated on a running system")
	}
	self := os.Getpid()
	for _, p := range procs {
		if p.PID == self {
			return
		}
	}
	t.Errorf("own pid %d not found among %d enumerated processes", self, len(procs))
}
