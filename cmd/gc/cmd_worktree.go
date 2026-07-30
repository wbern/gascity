package main

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/spf13/cobra"
)

var (
	worktreeResolveCity         = resolveCity
	worktreeLoadCityConfig      = loadCityConfig
	worktreeOpenCityStoreAt     = openCityStoreAt
	worktreeListAllSessionBeads = session.ListAllSessionBeads
	worktreeScanStrayWorktrees  = scanStrayWorktrees
	worktreeOpenRigStore        = openStoreAtForCity
	worktreeLiveWorkerDirsFn    = worktreeLiveWorkerDirs

	// worktreeReapClosedBeadWorktrees is the controller's own reaper, reached
	// through a var so the command's wiring and rendering are testable without
	// standing up rigs, git worktrees and bead stores. The reaper itself is
	// upstream-owned and unchanged by this command.
	worktreeReapClosedBeadWorktrees = reapClosedBeadWorktrees
)

func newWorktreeCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "worktree",
		Short: "Manage worktree state",
		Long: `Manage worktree state.

Provides report-only and maintenance operations over the city's managed
worktree roots.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				fmt.Fprintln(stderr, "gc worktree: missing subcommand (scan, reap)") //nolint:errcheck // best-effort stderr
			} else {
				fmt.Fprintf(stderr, "gc worktree: unknown subcommand %q\n", args[0]) //nolint:errcheck // best-effort stderr
			}
			return errExit
		},
	}
	cmd.AddCommand(newWorktreeScanCmd(stdout, stderr))
	cmd.AddCommand(newWorktreeReapCmd(stdout, stderr))
	return cmd
}

// newWorktreeReapCmd deliberately offers no --json flag. The CLI's JSON
// contract requires a checked-in schema under schemas/<command path>/ and an
// {schema_version, ok, ...} envelope; without both, --json aborts with
// "does not declare JSON support" no matter what the command writes. Advertising
// the flag before the schema exists would ship exactly the sort of
// looks-wired-but-inert surface this command was built to expose. Tracked
// separately, together with the same pre-existing breakage in `worktree scan`.
func newWorktreeReapCmd(stdout, stderr io.Writer) *cobra.Command {
	var execute bool
	cmd := &cobra.Command{
		Use:   "reap",
		Short: "Report (or perform) closed-bead worktree reclamation",
		Long: `Report what the closed-bead worktree reaper would reclaim.

Runs the controller's own reaper — the same gates, in the same order — and
prints each decision with its reason. Dry-run is the default; nothing is
removed unless --execute is passed.

This exists so the reaper is answerable. Its gates otherwise run only inside
the controller tick behind a config flag, so "what would be reclaimed, and
why is that tree being kept" had no operator-facing answer.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if doWorktreeReap(args, execute, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&execute, "execute", false, "Actually remove the worktrees (default is dry-run)")
	return cmd
}

// doWorktreeReap runs the reaper over every rig in the city and renders the
// report. Dry-run is the default because the alternative — a missing flag
// meaning "delete a few hundred worktrees" — is not a safe default for a verb
// an operator or an order may run by mistake.
func doWorktreeReap(args []string, execute bool, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintf(stderr, "gc worktree reap: unexpected arguments: %s\n", strings.Join(args, " ")) //nolint:errcheck // best-effort stderr
		return 1
	}

	cityPath, err := worktreeResolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc worktree reap: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cfg, err := worktreeLoadCityConfig(cityPath, io.Discard)
	if err != nil {
		fmt.Fprintf(stderr, "gc worktree reap: loading city config: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// The liveness gate is only as good as the live-session set handed to it:
	// an empty set turns "protect trees a live session is working in" into a
	// no-op, which is the shape of the would-reap-19-live incident.
	liveSet, err := worktreeLiveWorkerDirsFn(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc worktree reap: loading live session set: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	liveDirs := make([]string, 0, len(liveSet))
	for dir := range liveSet {
		liveDirs = append(liveDirs, dir)
	}
	sort.Strings(liveDirs)

	stores, err := worktreeReapRigStores(cityPath, cfg, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc worktree reap: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if len(stores) == 0 {
		fmt.Fprintln(stderr, "gc worktree reap: no rig bead stores could be opened; nothing to inspect") //nolint:errcheck // best-effort stderr
		return 1
	}

	// Reaper diagnostics go to stderr so stdout stays a clean report.
	report := worktreeReapClosedBeadWorktrees(cityPath, cfg, stores, liveDirs, !execute, events.Discard, stderr)

	renderReapReport(stdout, report)
	return 0
}

// worktreeReapRigStores opens one bead store per bound rig. A rig whose store
// cannot be opened is reported and skipped rather than failing the whole run:
// the reaper is per-rig, and one unreadable rig should not hide the others'
// findings. An unbound rig (no path) has no store to open.
func worktreeReapRigStores(cityPath string, cfg *config.City, stderr io.Writer) (map[string]beads.Store, error) {
	if cfg == nil {
		return nil, fmt.Errorf("no city config")
	}
	resolveRigPaths(cityPath, cfg.Rigs)
	stores := make(map[string]beads.Store, len(cfg.Rigs))
	for _, rig := range cfg.Rigs {
		rigPath := strings.TrimSpace(rig.Path)
		if rigPath == "" {
			fmt.Fprintf(stderr, "gc worktree reap: rig %q is unbound (no path); skipping\n", rig.Name) //nolint:errcheck // best-effort stderr
			continue
		}
		store, err := worktreeOpenRigStore(rigPath, cityPath)
		if err != nil {
			fmt.Fprintf(stderr, "gc worktree reap: rig %q: opening bead store: %v; skipping\n", rig.Name, err) //nolint:errcheck // best-effort stderr
			continue
		}
		stores[rig.Name] = store
	}
	return stores, nil
}

// renderReapReport prints the decisions and a one-line summary. Protected trees
// are printed with their reason — that column is the whole point, because "the
// reaper reclaimed nothing" is only actionable when it says why.
func renderReapReport(stdout io.Writer, report reapReport) {
	mode := "reaped"
	verb := "reaped"
	if report.DryRun {
		mode = "dry-run: would reap"
		verb = "would reap"
	}

	if len(report.Reaped) > 0 {
		fmt.Fprintf(stdout, "%s:\n", mode) //nolint:errcheck // best-effort stdout
		tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "BEAD\tRIG\tBRANCH\tPATH\tWARNING") //nolint:errcheck // best-effort stdout
		for _, d := range report.Reaped {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", d.BeadID, d.Rig, d.Branch, d.Path, d.Warning) //nolint:errcheck // best-effort stdout
		}
		_ = tw.Flush()
	}

	if len(report.Protected) > 0 {
		fmt.Fprintln(stdout, "protected:") //nolint:errcheck // best-effort stdout
		tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "BEAD\tRIG\tPATH\tREASON") //nolint:errcheck // best-effort stdout
		for _, d := range report.Protected {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", d.BeadID, d.Rig, d.Path, d.Reason) //nolint:errcheck // best-effort stdout
		}
		_ = tw.Flush()
	}

	suffix := ""
	if report.DryRun {
		suffix = " (dry-run: nothing was removed)"
	}
	// Call out unlanded work separately. Every other protection describes a
	// tree that reproduces from the remote, so a bare kept-count reads the
	// same whether the fleet is merely busy or is sitting on commits that
	// exist nowhere else — and the latter accumulates silently.
	stranded := ""
	if n := countHoldingUnlandedWork(report.Protected); n > 0 {
		stranded = fmt.Sprintf(", %d holding unlanded work", n)
	}
	fmt.Fprintf(stdout, "%d %s, %d kept%s%s\n", len(report.Reaped), verb, len(report.Protected), stranded, suffix) //nolint:errcheck // best-effort stdout
}

// countHoldingUnlandedWork returns how many protected decisions are held
// because they carry commits no remote carries.
func countHoldingUnlandedWork(protected []reapDecision) int {
	n := 0
	for _, d := range protected {
		if d.HoldsUnlandedWork {
			n++
		}
	}
	return n
}

func newWorktreeScanCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonFlag bool
	cmd := &cobra.Command{
		Use:   "scan",
		Short: "List stray worktrees under managed roots",
		Args:  cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if doWorktreeScan(args, jsonFlag, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "Output in JSON format")
	return cmd
}

func doWorktreeScan(args []string, jsonFlag bool, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintf(stderr, "gc worktree scan: unexpected arguments: %s\n", strings.Join(args, " ")) //nolint:errcheck // best-effort stderr
		return 1
	}

	cityPath, err := worktreeResolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc worktree scan: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cfg, err := worktreeLoadCityConfig(cityPath, io.Discard)
	if err != nil {
		fmt.Fprintf(stderr, "gc worktree scan: loading city config: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	roots := worktreeManagedRoots(cityPath, cfg)
	liveSet, err := worktreeLiveWorkerDirs(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc worktree scan: loading live session set: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	strays, err := worktreeScanStrayWorktrees(roots, liveSet, newGitProbe)
	if err != nil {
		fmt.Fprintf(stderr, "gc worktree scan: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	sortStrayWorktrees(strays)

	if jsonFlag {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(strays); err != nil {
			fmt.Fprintf(stderr, "gc worktree scan: writing json: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return 0
	}

	renderStrayWorktreesTable(stdout, strays)
	return 0
}

func worktreeManagedRoots(cityPath string, cfg *config.City) []string {
	roots := []string{filepath.Join(cityPath, ".gc", "worktrees")}
	if cfg == nil {
		return roots
	}
	resolveRigPaths(cityPath, cfg.Rigs)
	for _, rig := range cfg.Rigs {
		rigPath := strings.TrimSpace(rig.Path)
		if rigPath == "" {
			continue
		}
		roots = append(roots, filepath.Clean(rigPath))
	}
	seen := make(map[string]struct{}, len(roots))
	out := make([]string, 0, len(roots))
	for _, root := range roots {
		root = filepath.Clean(root)
		if _, ok := seen[root]; ok {
			continue
		}
		seen[root] = struct{}{}
		out = append(out, root)
	}
	return out
}

func worktreeLiveWorkerDirs(cityPath string) (map[string]bool, error) {
	store, err := worktreeOpenCityStoreAt(cityPath)
	if err != nil {
		return nil, err
	}
	list, err := worktreeListAllSessionBeads(store, beads.ListQuery{})
	if err != nil {
		return nil, err
	}

	liveSet := make(map[string]bool, len(list))
	for _, b := range list {
		if b.Status == "closed" {
			continue
		}
		dir := contract.WorkerDirFromMetadata(b.Metadata)
		if !filepath.IsAbs(dir) {
			continue
		}
		liveSet[filepath.Clean(dir)] = true
	}
	return liveSet, nil
}

func sortStrayWorktrees(strays []strayWorktree) {
	sort.SliceStable(strays, func(i, j int) bool {
		if strays[i].Reclaimable != strays[j].Reclaimable {
			return strays[i].Reclaimable && !strays[j].Reclaimable
		}
		return strays[i].Path < strays[j].Path
	})
}

func renderStrayWorktreesTable(stdout io.Writer, strays []strayWorktree) {
	if len(strays) == 0 {
		fmt.Fprintln(stdout, "No stray worktrees found under managed roots.") //nolint:errcheck // best-effort stdout
		return
	}

	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RECLAIMABLE\tPATH\tREASON") //nolint:errcheck // best-effort stdout

	reclaimable := 0
	for _, stray := range strays {
		flag := "no"
		if stray.Reclaimable {
			flag = "yes"
			reclaimable++
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", flag, stray.Path, stray.Reason) //nolint:errcheck // best-effort stdout
	}
	_ = tw.Flush()

	fmt.Fprintf(stdout, "%d stray checkout(s): %d reclaimable, %d kept\n", len(strays), reclaimable, len(strays)-reclaimable) //nolint:errcheck // best-effort stdout
}
