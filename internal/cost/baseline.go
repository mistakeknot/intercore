package cost

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// BaselineOpts configures the cost-per-landable-change query.
type BaselineOpts struct {
	LastN           int    // last N shipped beads (default: 50, 0 = all)
	Days            int    // only beads closed within last N days (default: 30)
	ByPhase         bool   // break down by workflow phase
	ByAgent         bool   // break down by agent type
	JSON            bool   // output JSON
	InterstatScript string // path to cost-query.sh (override for testing)
	BeadsDir        string // BEADS_DIR override (for testing)
}

// BaselineResult holds the computed baseline metric.
type BaselineResult struct {
	Period       Period                `json:"period"`
	ShippedBeads int                   `json:"shipped_beads"`
	Stats        TokenStats            `json:"stats"`
	ByPhase      map[string]TokenStats `json:"by_phase,omitempty"`
	ByAgent      map[string]TokenStats `json:"by_agent,omitempty"`
	Uncorrelated *TokenStats           `json:"uncorrelated,omitempty"`
}

// Period describes the time window of the baseline.
type Period struct {
	Days  int    `json:"days"`
	Start string `json:"start,omitempty"`
	End   string `json:"end,omitempty"`
}

// TokenStats holds percentile and aggregate statistics.
type TokenStats struct {
	P50         int64 `json:"p50"`
	P90         int64 `json:"p90"`
	P95         int64 `json:"p95"`
	Mean        int64 `json:"mean"`
	Total       int64 `json:"total"`
	InputTotal  int64 `json:"input_total"`
	OutputTotal int64 `json:"output_total"`
	Count       int   `json:"count"`
}

// interstatRow represents a row from cost-query.sh by-bead or by-bead-phase output.
type interstatRow struct {
	BeadID       string `json:"bead_id"`
	Phase        string `json:"phase,omitempty"`
	Agent        string `json:"agent,omitempty"`
	Runs         int    `json:"runs"`
	Tokens       int64  `json:"tokens"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
}

// beadRecord represents a closed bead from bd list --json.
type beadRecord struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Status   string `json:"status"`
	ClosedAt string `json:"closed_at"`
}

// ComputeBaseline runs the cost-per-landable-change analysis.
func ComputeBaseline(ctx context.Context, opts BaselineOpts) (*BaselineResult, error) {
	if opts.LastN == 0 {
		opts.LastN = 50
	}
	if opts.Days == 0 {
		opts.Days = 30
	}

	// 1. Get shipped beads
	beads, err := listShippedBeads(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("listing shipped beads: %w", err)
	}

	// Filter by time window
	cutoff := time.Now().AddDate(0, 0, -opts.Days)
	var filtered []beadRecord
	for _, b := range beads {
		t, err := time.Parse(time.RFC3339, b.ClosedAt)
		if err != nil {
			continue // skip unparseable dates
		}
		if t.After(cutoff) {
			filtered = append(filtered, b)
		}
	}

	// Apply LastN limit
	if opts.LastN > 0 && len(filtered) > opts.LastN {
		filtered = filtered[:opts.LastN]
	}

	// Build bead ID set for correlation
	beadIDs := make(map[string]bool, len(filtered))
	for _, b := range filtered {
		beadIDs[b.ID] = true
	}

	// 2. Query interstat token data (script path must be set by caller)
	scriptPath := opts.InterstatScript
	if scriptPath == "" {
		return nil, fmt.Errorf("InterstatScript is required (caller must resolve cost-query.sh path)")
	}

	result := &BaselineResult{
		Period: Period{
			Days:  opts.Days,
			Start: cutoff.Format("2006-01-02"),
			End:   time.Now().Format("2006-01-02"),
		},
		ShippedBeads: len(filtered),
	}

	// Agent breakdown is independent of bead correlation — query it early
	if opts.ByAgent {
		agentRows, err := queryInterstat(ctx, scriptPath, "aggregate")
		if err != nil {
			return nil, fmt.Errorf("querying interstat aggregate: %w", err)
		}

		result.ByAgent = make(map[string]TokenStats, len(agentRows))
		for _, row := range agentRows {
			result.ByAgent[row.Agent] = TokenStats{
				Total:       row.Tokens,
				InputTotal:  row.InputTokens,
				OutputTotal: row.OutputTokens,
				Count:       row.Runs,
				Mean:        safeDivide(row.Tokens, int64(row.Runs)),
			}
		}
	}

	// No beads → return result with agent breakdown only
	if len(filtered) == 0 {
		return result, nil
	}

	// 3. Get per-bead token totals
	beadRows, err := queryInterstat(ctx, scriptPath, "by-bead")
	if err != nil {
		return nil, fmt.Errorf("querying interstat by-bead: %w", err)
	}

	// Correlate: only include rows for shipped beads in window
	var perBeadTokens []int64
	var totalInput, totalOutput int64
	for _, row := range beadRows {
		if beadIDs[row.BeadID] {
			perBeadTokens = append(perBeadTokens, row.Tokens)
			totalInput += row.InputTokens
			totalOutput += row.OutputTokens
		}
	}

	result.Stats = computeStats(perBeadTokens, totalInput, totalOutput)

	// 4. Optional phase breakdown (requires bead correlation)
	if opts.ByPhase {
		phaseRows, err := queryInterstat(ctx, scriptPath, "by-bead-phase")
		if err != nil {
			return nil, fmt.Errorf("querying interstat by-bead-phase: %w", err)
		}

		phaseTokens := make(map[string][]int64)
		phaseInput := make(map[string]int64)
		phaseOutput := make(map[string]int64)
		for _, row := range phaseRows {
			if beadIDs[row.BeadID] && row.Phase != "" {
				phaseTokens[row.Phase] = append(phaseTokens[row.Phase], row.Tokens)
				phaseInput[row.Phase] += row.InputTokens
				phaseOutput[row.Phase] += row.OutputTokens
			}
		}

		result.ByPhase = make(map[string]TokenStats, len(phaseTokens))
		for phase, tokens := range phaseTokens {
			result.ByPhase[phase] = computeStats(tokens, phaseInput[phase], phaseOutput[phase])
		}
	}

	return result, nil
}

// FormatText returns a human-readable baseline report.
func FormatText(r *BaselineResult) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Cost Baseline (%s to %s)\n", r.Period.Start, r.Period.End)
	fmt.Fprintf(&b, "══════════════════════════════════════\n")
	fmt.Fprintf(&b, "  Shipped beads:  %d\n", r.ShippedBeads)

	if r.Stats.Count == 0 {
		fmt.Fprintf(&b, "  Token data:     no correlated data yet\n")
		fmt.Fprintf(&b, "\n  Beads exist but no token data is tagged with bead IDs.\n")
		fmt.Fprintf(&b, "  Token tagging started with the interstat bead_id column.\n")
		fmt.Fprintf(&b, "  Run more sprints and re-check.\n")

		// Still show agent breakdown if requested (independent of bead correlation)
		if len(r.ByAgent) > 0 {
			fmt.Fprintf(&b, "\n  Global Agent Totals (all sessions):\n")
			agents := sortedKeys(r.ByAgent)
			for _, agent := range agents {
				s := r.ByAgent[agent]
				fmt.Fprintf(&b, "    %-30s %s (%d runs)\n", agent, formatTokens(s.Total), s.Count)
			}
		}
		return b.String()
	}

	fmt.Fprintf(&b, "  Beads w/ data:  %d\n", r.Stats.Count)
	fmt.Fprintf(&b, "\n")
	fmt.Fprintf(&b, "  Cost per landed change (tokens):\n")
	fmt.Fprintf(&b, "    p50:   %s\n", formatTokens(r.Stats.P50))
	fmt.Fprintf(&b, "    p90:   %s\n", formatTokens(r.Stats.P90))
	fmt.Fprintf(&b, "    p95:   %s\n", formatTokens(r.Stats.P95))
	fmt.Fprintf(&b, "    mean:  %s\n", formatTokens(r.Stats.Mean))
	fmt.Fprintf(&b, "    total: %s\n", formatTokens(r.Stats.Total))
	fmt.Fprintf(&b, "    (input: %s, output: %s)\n", formatTokens(r.Stats.InputTotal), formatTokens(r.Stats.OutputTotal))

	if len(r.ByPhase) > 0 {
		fmt.Fprintf(&b, "\n  By Phase:\n")
		phases := sortedKeys(r.ByPhase)
		for _, phase := range phases {
			s := r.ByPhase[phase]
			fmt.Fprintf(&b, "    %-20s %s (%d runs)\n", phase, formatTokens(s.Total), s.Count)
		}
	}

	if len(r.ByAgent) > 0 {
		fmt.Fprintf(&b, "\n  By Agent:\n")
		agents := sortedKeys(r.ByAgent)
		for _, agent := range agents {
			s := r.ByAgent[agent]
			fmt.Fprintf(&b, "    %-30s %s (%d runs)\n", agent, formatTokens(s.Total), s.Count)
		}
	}

	return b.String()
}

// queryInterstat runs cost-query.sh with the given mode and parses JSON output.
func queryInterstat(ctx context.Context, scriptPath, mode string) ([]interstatRow, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", scriptPath, mode)
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("%s %s failed: %s", scriptPath, mode, string(exitErr.Stderr))
		}
		return nil, err
	}

	// Empty output or "[]" → no data
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" || trimmed == "[]" {
		return nil, nil
	}

	var rows []interstatRow
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, fmt.Errorf("parsing %s output: %w", mode, err)
	}
	return rows, nil
}

// listShippedBeads queries bd for closed beads, sorted newest first.
func listShippedBeads(ctx context.Context, opts BaselineOpts) ([]beadRecord, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	bdPath, err := exec.LookPath("bd")
	if err != nil {
		return nil, fmt.Errorf("bd not found in PATH: %w", err)
	}

	args := []string{"list", "--status=closed", "--json", "--limit", "0"}
	cmd := exec.CommandContext(ctx, bdPath, args...)
	if opts.BeadsDir != "" {
		cmd.Env = append(os.Environ(), "BEADS_DIR="+opts.BeadsDir)
	}

	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("bd list failed: %s", string(exitErr.Stderr))
		}
		return nil, err
	}

	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" || trimmed == "[]" {
		return nil, nil
	}

	var beads []beadRecord
	if err := json.Unmarshal(out, &beads); err != nil {
		return nil, fmt.Errorf("parsing bd output: %w", err)
	}

	// Sort newest first (by closed_at descending)
	sort.Slice(beads, func(i, j int) bool {
		return beads[i].ClosedAt > beads[j].ClosedAt
	})

	return beads, nil
}

// computeStats computes percentiles and aggregates from a token values slice.
func computeStats(values []int64, inputTotal, outputTotal int64) TokenStats {
	if len(values) == 0 {
		return TokenStats{}
	}

	sorted := make([]int64, len(values))
	copy(sorted, values)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	n := len(sorted)
	var total int64
	for _, v := range sorted {
		total += v
	}

	return TokenStats{
		P50:         sorted[min(n*50/100, n-1)],
		P90:         sorted[min(n*90/100, n-1)],
		P95:         sorted[min(n*95/100, n-1)],
		Mean:        total / int64(n),
		Total:       total,
		InputTotal:  inputTotal,
		OutputTotal: outputTotal,
		Count:       n,
	}
}

// FindInterstatScript searches standard locations for cost-query.sh.
// Lives at the CLI layer (exported for cmd/ic) to avoid L1→L3 coupling.
func FindInterstatScript() string {
	candidates := []string{
		// Monorepo location (relative to ic binary)
		findRelativeToSelf("../../interverse/interstat/scripts/cost-query.sh"),
		// Plugin cache location
		filepath.Join(os.Getenv("HOME"), ".claude/plugins/cache/interagency-marketplace/interstat"),
	}

	for _, c := range candidates {
		if c == "" {
			continue
		}
		// For plugin cache, look for the script inside any version dir
		if strings.Contains(c, "plugins/cache") {
			script := findInPluginCache(c)
			if script != "" {
				return script
			}
			continue
		}
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// findInPluginCache finds cost-query.sh in the highest semver version directory.
func findInPluginCache(cacheDir string) string {
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return ""
	}

	// Sort by semver (not lexicographic) to pick highest version
	type versioned struct {
		name  string
		parts [3]int
	}
	var versions []versioned
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		v := parseVersion(e.Name())
		if v != [3]int{} {
			versions = append(versions, versioned{name: e.Name(), parts: v})
		}
	}
	sort.Slice(versions, func(i, j int) bool {
		a, b := versions[i].parts, versions[j].parts
		if a[0] != b[0] {
			return a[0] > b[0]
		}
		if a[1] != b[1] {
			return a[1] > b[1]
		}
		return a[2] > b[2]
	})

	for _, v := range versions {
		p := filepath.Join(cacheDir, v.name, "scripts", "cost-query.sh")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// parseVersion extracts major.minor.patch from a version string like "0.2.6".
func parseVersion(s string) [3]int {
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return [3]int{}
	}
	var v [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return [3]int{}
		}
		v[i] = n
	}
	return v
}

// findRelativeToSelf resolves a path relative to the ic binary's location.
func findRelativeToSelf(rel string) string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return ""
	}
	return filepath.Join(filepath.Dir(exe), rel)
}

func formatTokens(n int64) string {
	if n >= 1_000_000 {
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
	if n >= 1_000 {
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	}
	return fmt.Sprintf("%d", n)
}

func safeDivide(a, b int64) int64 {
	if b == 0 {
		return 0
	}
	return a / b
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
