package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// reapStoreScope is explicit because a worktree may be borrowed by a bead in a
// different rig or the city store. An omitted store is unknown, never empty.
type reapStoreScope struct {
	RigStores map[string]beads.Store
	CityStore beads.Store
}

// collectReapBorrowScope validates the required scope, then asks the canonical
// residency resolver for every work/rig/relocated-class leg. No suspension
// filtering is allowed: paused work may still borrow a checkout. The resolver
// deduplicates shared stores; each leg is live-read once, never per candidate.
// Any partial read vetoes deletion, regardless of the leg's softer census policy.
func collectReapBorrowScope(cityPath string, cfg *config.City, scope reapStoreScope, stderr io.Writer) (map[string][]string, []string) {
	if stderr == nil {
		stderr = io.Discard
	}
	refs := make(map[string][]string)
	var incomplete []string
	if cfg == nil {
		return refs, []string{"configuration"}
	}
	if scope.CityStore == nil {
		incomplete = append(incomplete, "city")
	}
	// Validate constructor inputs; leg selection remains the resolver's.
	for _, rig := range cfg.Rigs {
		if scope.RigStores[rig.Name] == nil {
			incomplete = append(incomplete, "rig:"+rig.Name)
		}
	}
	if len(incomplete) > 0 {
		sort.Strings(incomplete)
		fmt.Fprintf(stderr, "reapClosedBeadWorktrees: required borrow scopes unavailable: %v\n", incomplete) //nolint:errcheck
		return refs, incomplete
	}
	candidates, err := censusStoreCandidates(cityPath, cfg, scope.CityStore, scope.RigStores, nil, censusRefScoped)
	if err != nil {
		fmt.Fprintf(stderr, "reapClosedBeadWorktrees: borrow residency unresolved: %v\n", err) //nolint:errcheck
		return refs, []string{"residency"}
	}
	if len(candidates) == 0 {
		return refs, []string{"residency:no-stores"}
	}
	for _, candidate := range candidates {
		name := candidate.ref
		if candidate.store == nil {
			incomplete = append(incomplete, name)
			continue
		}
		found, err := scanBorrowVetoReferences(candidate.store, nil)
		if err != nil {
			incomplete = append(incomplete, name)
			fmt.Fprintf(stderr, "reapClosedBeadWorktrees: borrow scope %s unreadable: %v\n", name, err) //nolint:errcheck
			continue
		}
		for path, ids := range found {
			for _, id := range ids {
				refs[path] = append(refs[path], name+"/"+id)
			}
		}
	}
	sort.Strings(incomplete)
	return refs, incomplete
}

// Resolve the physical prefix before lexical .. cleanup. Abs/Clean first can
// turn alias/.. into the wrong directory when alias names a symlink. Missing
// tails retain their absolute location; permission errors and cycles refuse.
func normalizeReapBorrowPath(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("work-dir must be absolute")
	}
	resolved, err := fsys.ResolveSymlinks(fsys.OSFS{}, path)
	if err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}

func reapBorrowersUnder(index map[string][]string, path string) ([]string, error) {
	root, err := normalizeReapBorrowPath(path)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	for borrower, ids := range index {
		relative, err := filepath.Rel(root, borrower)
		if err != nil {
			return nil, err
		}
		if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
			continue
		}
		for _, id := range ids {
			seen[id] = true
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

// reapOwnerLive bypasses cached Get state: reopening a closed bead immediately
// revokes eligibility even when it carries no work-directory metadata.
func reapOwnerLive(store beads.Store, id string) (beads.Bead, error) {
	if store == nil {
		return beads.Bead{}, fmt.Errorf("owner store unavailable")
	}
	rows, err := store.List(beads.ListQuery{IDs: []string{id}, IncludeClosed: true, TierMode: beads.TierBoth, Live: true})
	if err != nil {
		return beads.Bead{}, err
	}
	if len(rows) != 1 || rows[0].ID != id {
		return beads.Bead{}, fmt.Errorf("owner missing or ambiguous")
	}
	return rows[0], nil
}

// Discover every populated managed namespace before any removal. An empty
// non-Git control rig has no checkout payload and needs no Git enumeration.
// A missing/broken repository behind a populated namespace is incomplete.
func discoverReapNamespaces(cfg *config.City, scope reapStoreScope, root string, live liveWorktreeState, sessionDirs []string, stderr io.Writer) (map[string][]worktreeLiveness, []string) {
	found := make(map[string][]worktreeLiveness)
	var incomplete []string
	for rigName := range scope.RigStores {
		dir := filepath.Join(root, rigName)
		entries, err := os.ReadDir(dir)
		if os.IsNotExist(err) || (err == nil && len(entries) == 0) {
			continue
		}
		name := "discovery:" + rigName
		if err != nil {
			incomplete = append(incomplete, name)
			fmt.Fprintf(stderr, "reapClosedBeadWorktrees: managed namespace %s unreadable: %v\n", rigName, err) //nolint:errcheck
			continue
		}
		rigRoot := rigRootByName(cfg, rigName)
		if rigRoot == "" {
			incomplete = append(incomplete, name)
			continue
		}
		inventory, err := discoverWorktreeLiveness(rigRoot, live, sessionDirs)
		if err != nil {
			incomplete = append(incomplete, name)
			fmt.Fprintf(stderr, "reapClosedBeadWorktrees: managed discovery %s failed: %v\n", rigName, err) //nolint:errcheck
			continue
		}
		found[rigName] = inventory
	}
	sort.Strings(incomplete)
	return found, incomplete
}
