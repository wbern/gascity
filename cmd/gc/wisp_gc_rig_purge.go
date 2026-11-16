package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// purgeRigStoresEnv selects whether the wisp GC purges expired closed workflow
// roots from rig stores (the city-store purge never sees them). Values: "off",
// "dry-run" (the default: log what would be deleted, delete nothing) and "on".
const purgeRigStoresEnv = "GC_WISP_GC_PURGE_RIG_STORES"

// rigPurgeMode is the parsed purgeRigStoresEnv value.
type rigPurgeMode string

// The rig-store purge modes.
const (
	rigPurgeOff    rigPurgeMode = "off"
	rigPurgeDryRun rigPurgeMode = "dry-run"
	rigPurgeOn     rigPurgeMode = "on"
)

// parseRigPurgeMode maps an env value to a mode; anything but "off" or "on"
// (including unset) is the non-mutating dry-run.
func parseRigPurgeMode(v string) rigPurgeMode {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "off":
		return rigPurgeOff
	case "on":
		return rigPurgeOn
	default:
		return rigPurgeDryRun
	}
}

// rigPurgeModeFn reads the mode; a package var so tests can flip it.
var rigPurgeModeFn = func() rigPurgeMode {
	return parseRigPurgeMode(os.Getenv(purgeRigStoresEnv))
}

// purgeClosedRigRoots runs the closed-root purge of the city graph store
// (closedWispGCEntries + purgeExpiredBeadClosures, same ttl, same
// wispGCClosurePurgeBatchCap) against every rig store. In dry-run mode it
// deletes nothing and logs, per rig, the roots and members it would delete. A
// failing rig does not stop the others; its error is returned joined.
func purgeClosedRigRoots(ctx context.Context, rigStores map[string]beads.Store, ttl time.Duration, now time.Time) (int, error) {
	mode := rigPurgeModeFn()
	if mode == rigPurgeOff || ttl <= 0 {
		return 0, nil
	}
	names := make([]string, 0, len(rigStores))
	for name := range rigStores {
		names = append(names, name)
	}
	slices.Sort(names)
	purged := 0
	var sweepErr error
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return purged, errors.Join(sweepErr, fmt.Errorf("rig purge abandoned before rig %s: %w", name, err))
		}
		start := time.Now()
		n, err := purgeClosedRigRoot(rigStores[name], name, mode, now.Add(-ttl))
		purged += n
		log.Printf("wisp gc: rig closed-root purge rig=%s mode=%s purged=%d took=%s", name, mode, n, time.Since(start).Round(time.Millisecond))
		if err != nil {
			sweepErr = errors.Join(sweepErr, fmt.Errorf("rig %s: %w", name, err))
		}
	}
	return purged, sweepErr
}

// purgeClosedRigRoot is purgeClosedRigRoots for one rig store.
func purgeClosedRigRoot(store beads.Store, rig string, mode rigPurgeMode, cutoff time.Time) (int, error) {
	if store == nil {
		return 0, nil
	}
	entries, err := closedWispGCEntries(store)
	if err != nil {
		return 0, fmt.Errorf("listing closed roots: %w", err)
	}
	referenced, err := openBeadReferencedRoots(store)
	if err != nil {
		// Fail closed: references from live beads cannot be ruled out.
		return 0, fmt.Errorf("listing roots referenced by open beads: %w", err)
	}
	candidates := entries[:0:0]
	held := 0
	for _, e := range entries {
		if _, ok := referenced[e.ID]; ok {
			held++
			continue
		}
		candidates = append(candidates, e)
	}
	if mode == rigPurgeOn {
		if held > 0 {
			log.Printf("wisp gc: rig closed-root purge rig=%s kept %d closed root(s) referenced by open beads", rig, held)
		}
		return purgeExpiredBeadClosures(store, candidates, cutoff, wispGCClosurePurgeBatchCap)
	}
	roots, members, err := countRigPurgeCandidates(store, candidates, cutoff, wispGCClosurePurgeBatchCap)
	log.Printf("wisp gc: rig closed-root purge rig=%s mode=%s would_delete_roots=%d would_delete_members=%d held_by_open_refs=%d", rig, mode, roots, members, held)
	return 0, err
}

// countRigPurgeCandidates walks, read-only, the closures of up to batchCap
// expired roots and counts the roots whose closure is entirely closed and the
// members a purge would delete.
func countRigPurgeCandidates(store beads.Store, entries []beads.Bead, cutoff time.Time, batchCap int) (roots, members int, err error) {
	walked := 0
	for _, e := range entries {
		if batchCap > 0 && walked >= batchCap {
			break
		}
		if e.CreatedAt.IsZero() || !e.CreatedAt.Before(cutoff) {
			continue
		}
		walked++
		ids, open, _, walkErr := collectExpiredBeadClosureWithOpen(store, e)
		if walkErr != nil {
			err = errors.Join(err, fmt.Errorf("walking closure of %q: %w", e.ID, walkErr))
			continue
		}
		if len(open) > 0 {
			continue
		}
		roots++
		members += len(ids)
	}
	return roots, members, err
}

// openBeadReferencedRoots returns the ids that a live (open or in_progress)
// bead still points at through molecule_id, gc.workflow_id, gc.root_bead_id or
// its parent. Deleting such a root would strand the live bead, so the rig
// purge keeps it; the closure purge itself guards only open closure members
// and open gc.root_bead_id members.
func openBeadReferencedRoots(store beads.Store) (map[string]struct{}, error) {
	refs := make(map[string]struct{})
	for _, status := range []string{"open", "in_progress"} {
		live, err := store.List(beads.ListQuery{Status: status, TierMode: beads.TierBoth})
		if err != nil {
			return nil, fmt.Errorf("listing %s beads: %w", status, err)
		}
		for _, b := range live {
			for _, key := range []string{beadmeta.MoleculeIDMetadataKey, beadmeta.WorkflowIDMetadataKey, beadmeta.RootBeadIDMetadataKey} {
				if id := strings.TrimSpace(b.Metadata[key]); id != "" {
					refs[id] = struct{}{}
				}
			}
			if id := stepParentID(b); id != "" {
				refs[id] = struct{}{}
			}
		}
	}
	return refs, nil
}
