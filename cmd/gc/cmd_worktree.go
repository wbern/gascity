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
	"github.com/gastownhall/gascity/internal/session"
	"github.com/spf13/cobra"
)

var (
	worktreeResolveCity         = resolveCity
	worktreeLoadCityConfig      = loadCityConfig
	worktreeOpenCityStoreAt     = openCityStoreAt
	worktreeListAllSessionBeads = session.ListAllSessionBeads
	worktreeScanStrayWorktrees  = scanStrayWorktrees
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
				fmt.Fprintln(stderr, "gc worktree: missing subcommand (scan)") //nolint:errcheck // best-effort stderr
			} else {
				fmt.Fprintf(stderr, "gc worktree: unknown subcommand %q\n", args[0]) //nolint:errcheck // best-effort stderr
			}
			return errExit
		},
	}
	cmd.AddCommand(newWorktreeScanCmd(stdout, stderr))
	return cmd
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
