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
	"github.com/gastownhall/gascity/internal/worktree"
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

// newWorktreeCmd returns the gc worktree command group. It is the CLI face
// of internal/worktree — the transactional workspace owner (gc-r9fx) that
// sling and formula-managed workspace setup can route through instead of
// running competing ad hoc provisioning — plus the report-only and
// maintenance operations (scan, reap) over the city's managed worktree roots.
func newWorktreeCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "worktree",
		Short: "Ensure, verify, and maintain agent workspace worktrees",
		Long: `Ensure, verify, and maintain agent workspace worktrees.

gc worktree is the single transactional owner for workspace provisioning.
Postconditions: the path is a direct child of the configured per-rig root and
the root of a worktree of the given repository, with the bead's uniquely named
branch checked out on an attached HEAD (never detached). Durable provenance is
stored in the worktree's private git directory and returned as JSON so callers
can atomically publish the same evidence on the bead. A new branch is created
from --base, resolved verbatim against the local repository. Failed creation
rolls back everything it created; --dry-run plans without mutating anything.

It also provides report-only and maintenance operations over the city's
managed worktree roots (scan, reap).`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				fmt.Fprintln(stderr, "gc worktree: missing subcommand (ensure, verify, cleanup, scan, reap)") //nolint:errcheck // best-effort stderr
			} else {
				fmt.Fprintf(stderr, "gc worktree: unknown subcommand %q\n", args[0]) //nolint:errcheck // best-effort stderr
			}
			return errExit
		},
	}
	cmd.AddCommand(newWorktreeEnsureCmd(stdout, stderr))
	cmd.AddCommand(newWorktreeVerifyCmd(stdout, stderr))
	cmd.AddCommand(newWorktreeCleanupCmd(stdout, stderr))
	cmd.AddCommand(newWorktreeScanCmd(stdout, stderr))
	cmd.AddCommand(newWorktreeReapCmd(stdout, stderr))
	return cmd
}

// newWorktreeReapCmd offers --json against a checked-in schema at
// schemas/worktree/reap/. The CLI's JSON contract requires both that schema and
// an {schema_version, ok, ...} envelope; without either, --json aborts with
// "does not declare JSON support" no matter what the command writes, which is
// how `worktree scan` shipped an inert --json flag for its whole life.
func newWorktreeReapCmd(stdout, stderr io.Writer) *cobra.Command {
	var execute bool
	var jsonFlag bool
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
			if doWorktreeReap(args, execute, jsonFlag, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&execute, "execute", false, "Actually remove the worktrees (default is dry-run)")
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "Output in JSON format")
	return cmd
}

// doWorktreeReap runs the reaper over every rig in the city and renders the
// report. Dry-run is the default because the alternative — a missing flag
// meaning "delete a few hundred worktrees" — is not a safe default for a verb
// an operator or an order may run by mistake.
func doWorktreeReap(args []string, execute, jsonFlag bool, stdout, stderr io.Writer) int {
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
	report := worktreeReapClosedBeadWorktrees(cityPath, cfg, stores, liveDirs, !execute, events.Discard, nil, stderr)

	if jsonFlag {
		if err := encodeWorktreeJSON(stdout, newWorktreeReapJSON(report)); err != nil {
			fmt.Fprintf(stderr, "gc worktree reap: writing json: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return 0
	}

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
		payload := worktreeScanJSON{
			SchemaVersion: worktreeJSONSchemaVersion,
			OK:            true,
			Strays:        make([]worktreeStrayJSON, 0, len(strays)),
		}
		for _, s := range strays {
			payload.Strays = append(payload.Strays, worktreeStrayJSON(s))
		}
		if err := encodeWorktreeJSON(stdout, payload); err != nil {
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

// worktreeCmdOpts carries the flag values for gc worktree subcommands.
type worktreeCmdOpts struct {
	Repo       string
	Root       string
	Path       string
	Branch     string
	Base       string
	BaseSHA    string
	BeadID     string
	StoreRef   string
	Creator    string
	Owner      string
	Generation string
	Lifecycle  string
	AttemptID  string
	DryRun     bool
	JSON       bool
}

func worktreeFlagSet(cmd *cobra.Command, opts *worktreeCmdOpts) {
	cmd.Flags().StringVar(&opts.Repo, "repo", "", "repository directory the worktree belongs to (required)")
	cmd.Flags().StringVar(&opts.Root, "root", "", "configured per-rig worktree root; path must be its direct child (required)")
	cmd.Flags().StringVar(&opts.Path, "path", "", "worktree path (required)")
	cmd.Flags().StringVar(&opts.Branch, "branch", "", "branch that must be checked out (required)")
	cmd.Flags().StringVar(&opts.Base, "base", "", "exact base ref used for this worktree (required)")
	cmd.Flags().StringVar(&opts.BaseSHA, "base-sha", "", "recorded base SHA to verify when reusing a worktree")
	cmd.Flags().StringVar(&opts.BeadID, "bead", "", "work bead bound to this worktree (required)")
	cmd.Flags().StringVar(&opts.StoreRef, "store-ref", "", "work bead store reference (required)")
	cmd.Flags().StringVar(&opts.Creator, "creator", "", "mechanism creating the worktree (required)")
	cmd.Flags().StringVar(&opts.Owner, "owner", "", "single selected provisioning owner (required)")
	cmd.Flags().StringVar(&opts.Generation, "generation", "", "provisioning generation fence (required)")
	cmd.Flags().StringVar(&opts.Lifecycle, "lifecycle", worktree.LifecycleActive, "worktree lifecycle state")
	cmd.Flags().BoolVar(&opts.JSON, "json", false, "emit the report as JSON")
	for _, name := range []string{
		"repo", "root", "path", "branch", "base", "bead", "store-ref", "creator", "owner", "generation",
	} {
		_ = cmd.MarkFlagRequired(name) //nolint:errcheck // flags exist
	}
}

func newWorktreeEnsureCmd(stdout, stderr io.Writer) *cobra.Command {
	var opts worktreeCmdOpts
	cmd := &cobra.Command{
		Use:   "ensure",
		Short: "Ensure the worktree exists and satisfies all postconditions",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if runWorktreeEnsure(opts, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	worktreeFlagSet(cmd, &opts)
	cmd.Flags().BoolVarP(&opts.DryRun, "dry-run", "n", false, "plan without mutating anything")
	return cmd
}

func newWorktreeVerifyCmd(stdout, stderr io.Writer) *cobra.Command {
	var opts worktreeCmdOpts
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Verify the worktree satisfies all postconditions without mutating",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if runWorktreeVerify(opts, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	worktreeFlagSet(cmd, &opts)
	return cmd
}

func newWorktreeCleanupCmd(stdout, stderr io.Writer) *cobra.Command {
	var opts worktreeCmdOpts
	cmd := &cobra.Command{
		Use:   "cleanup",
		Short: "Remove an owned worktree after all safety gates pass",
		Long: `Remove an owned worktree after all safety gates pass.

Cleanup verifies the canonical repository, path, branch, and durable ownership
provenance before acting. It refuses dirty worktrees, commits reachable from no
branch, tag, or remote-tracking ref, and commits not merged into --base.
--attempt-id binds the removal to one exact provisioning attempt, so a stale
request cannot remove a workspace re-created at the same path. There is no
force mode and no recursive-filesystem fallback. An already-absent,
unregistered path is an idempotent success. With --json, safety refusals return
a structured cleanup_pending result for formula automation.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if runWorktreeCleanup(opts, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	worktreeFlagSet(cmd, &opts)
	cmd.Flags().StringVar(&opts.AttemptID, "attempt-id", "",
		"attempt id returned by the ensure that created this worktree (required)")
	_ = cmd.MarkFlagRequired("attempt-id")
	return cmd
}

func (o worktreeCmdOpts) spec() (worktree.Spec, error) {
	repo, err := filepath.Abs(o.Repo)
	if err != nil {
		return worktree.Spec{}, fmt.Errorf("resolving --repo %q: %w", o.Repo, err)
	}
	path, err := filepath.Abs(o.Path)
	if err != nil {
		return worktree.Spec{}, fmt.Errorf("resolving --path %q: %w", o.Path, err)
	}
	root, err := filepath.Abs(o.Root)
	if err != nil {
		return worktree.Spec{}, fmt.Errorf("resolving --root %q: %w", o.Root, err)
	}
	return worktree.Spec{
		RepoDir:    repo,
		Root:       root,
		Path:       path,
		Branch:     o.Branch,
		Base:       o.Base,
		BaseSHA:    o.BaseSHA,
		BeadID:     o.BeadID,
		StoreRef:   o.StoreRef,
		Creator:    o.Creator,
		Owner:      o.Owner,
		Generation: o.Generation,
		Lifecycle:  o.Lifecycle,
		AttemptID:  o.AttemptID,
		DryRun:     o.DryRun,
	}, nil
}

func runWorktreeEnsure(opts worktreeCmdOpts, stdout, stderr io.Writer) int {
	spec, err := opts.spec()
	if err != nil {
		fmt.Fprintf(stderr, "gc worktree ensure: %v\n", err) //nolint:errcheck
		return 1
	}
	rep, err := worktree.Ensure(spec)
	if err != nil {
		fmt.Fprintf(stderr, "gc worktree ensure: %v\n", err) //nolint:errcheck
		return 1
	}
	return writeWorktreeReport("ensure", rep, opts, stdout, stderr)
}

func runWorktreeVerify(opts worktreeCmdOpts, stdout, stderr io.Writer) int {
	spec, err := opts.spec()
	if err != nil {
		fmt.Fprintf(stderr, "gc worktree verify: %v\n", err) //nolint:errcheck
		return 1
	}
	rep, err := worktree.Verify(spec)
	if err != nil {
		fmt.Fprintf(stderr, "gc worktree verify: %v\n", err) //nolint:errcheck
		return 1
	}
	return writeWorktreeReport("verify", rep, opts, stdout, stderr)
}

func runWorktreeCleanup(opts worktreeCmdOpts, stdout, stderr io.Writer) int {
	spec, err := opts.spec()
	if err != nil {
		report := worktree.CleanupReport{
			Path:           opts.Path,
			Branch:         opts.Branch,
			CleanupPending: true,
			Error: &worktree.CleanupError{
				Code:     worktree.CleanupErrorInvalidSpec,
				Message:  err.Error(),
				ExitCode: 1,
			},
		}
		return writeWorktreeCleanupReport(report, opts, stdout, stderr)
	}
	report, cleanupErr := worktree.Cleanup(spec)
	code := writeWorktreeCleanupReport(report, opts, stdout, stderr)
	if cleanupErr != nil {
		return 1
	}
	return code
}

type worktreeJSONResult struct {
	SchemaVersion string `json:"schema_version"`
	OK            bool   `json:"ok"`
	Command       string `json:"command"`
	Action        string `json:"action"`
	worktree.Report
}

func writeWorktreeReport(action string, rep worktree.Report, opts worktreeCmdOpts, stdout, stderr io.Writer) int {
	if opts.JSON {
		result := worktreeJSONResult{
			SchemaVersion: "1",
			OK:            true,
			Command:       "worktree " + action,
			Action:        action,
			Report:        rep,
		}
		if err := json.NewEncoder(stdout).Encode(result); err != nil {
			fmt.Fprintf(stderr, "gc worktree: encoding report: %v\n", err) //nolint:errcheck
			return 1
		}
		return 0
	}
	switch {
	case len(rep.Planned) > 0:
		fmt.Fprintf(stdout, "would run (dry-run):\n") //nolint:errcheck
		for _, action := range rep.Planned {
			fmt.Fprintf(stdout, "  %s\n", action) //nolint:errcheck
		}
	case rep.Created:
		fmt.Fprintf(stdout, "created worktree %s on branch %s at %s\n", rep.Path, rep.Branch, rep.Head) //nolint:errcheck
	default:
		fmt.Fprintf(stdout, "worktree %s on branch %s at %s\n", rep.Path, rep.Branch, rep.Head) //nolint:errcheck
	}
	return 0
}

type worktreeCleanupJSONResult struct {
	SchemaVersion string `json:"schema_version"`
	OK            bool   `json:"ok"`
	Command       string `json:"command"`
	Action        string `json:"action"`
	worktree.CleanupReport
}

func writeWorktreeCleanupReport(rep worktree.CleanupReport, opts worktreeCmdOpts, stdout, stderr io.Writer) int {
	if opts.JSON {
		result := worktreeCleanupJSONResult{
			SchemaVersion: "1",
			OK:            rep.Error == nil,
			Command:       "worktree cleanup",
			Action:        "cleanup",
			CleanupReport: rep,
		}
		if err := json.NewEncoder(stdout).Encode(result); err != nil {
			fmt.Fprintf(stderr, "gc worktree cleanup: encoding report: %v\n", err) //nolint:errcheck
			return 1
		}
		if rep.Error != nil {
			fmt.Fprintf(stderr, "gc worktree cleanup: %s\n", rep.Error.Message) //nolint:errcheck
			return 1
		}
		return 0
	}
	if rep.Error != nil {
		fmt.Fprintf(stderr, "gc worktree cleanup: %s\n", rep.Error.Message) //nolint:errcheck
		return 1
	}
	switch {
	case rep.AlreadyAbsent:
		fmt.Fprintf(stdout, "worktree %s is already absent\n", rep.Path) //nolint:errcheck
	case rep.Removed:
		fmt.Fprintf(stdout, "removed worktree %s on branch %s\n", rep.Path, rep.Branch) //nolint:errcheck
	default:
		fmt.Fprintf(stdout, "worktree %s required no cleanup\n", rep.Path) //nolint:errcheck
	}
	return 0
}
