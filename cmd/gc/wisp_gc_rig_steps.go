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
	"github.com/gastownhall/gascity/internal/beads/closeorder"
)

// closeLeakedStepsEnv is the opt-in environment variable that lets the wisp GC
// close open steps whose molecule root is already closed. Like the other
// mutating wisp GC arms it DEFAULTS TO DRY-RUN: unset (or not truthy), the
// sweep only counts and logs the steps it would close.
const closeLeakedStepsEnv = "GC_WISP_GC_CLOSE_LEAKED_STEPS"

// leakedStepCloseReason is stamped on steps closed by the leaked-step sweep.
const leakedStepCloseReason = "wisp gc: open step of a closed molecule closed"

// wispGCLeakedStepBatchCap bounds how many leaked steps one sweep closes per
// rig store. A molecule's steps are never split across batches, so one sweep
// may exceed the cap by at most one molecule's steps.
var wispGCLeakedStepBatchCap = 500

// closeLeakedStepsEnforced reports whether the sweep closes (true) or only
// counts (false). Package var so tests can flip it without the environment.
var closeLeakedStepsEnforced = func() bool {
	return parseBoolEnv(os.Getenv(closeLeakedStepsEnv))
}

// leakedStepsResult reports one rig store's leaked-step sweep.
type leakedStepsResult struct {
	candidates int
	closed     int
	enforced   bool
}

// closeLeakedMoleculeSteps closes open steps whose parent molecule is already
// closed. A closer that closes only the molecule root (bd close does not
// cascade) leaves its steps open forever, and every open bead is scanned on
// every controller tick. Only an open, unassigned, non-exempt bead of type step
// whose parent is a closed molecule qualifies; in-progress, assigned, and
// non-step children are never touched.
func closeLeakedMoleculeSteps(store beads.Store, batchCap int) (leakedStepsResult, error) {
	res := leakedStepsResult{enforced: closeLeakedStepsEnforced()}
	if store == nil {
		return res, nil
	}
	roots, err := store.List(beads.ListQuery{Type: "molecule", Status: "closed", TierMode: beads.TierBoth})
	if err != nil {
		return res, fmt.Errorf("listing closed molecules: %w", err)
	}
	if len(roots) == 0 {
		return res, nil
	}
	closedRoots := make(map[string]struct{}, len(roots))
	for _, r := range roots {
		closedRoots[r.ID] = struct{}{}
	}
	steps, err := store.List(beads.ListQuery{Type: "step", Status: "open", TierMode: beads.TierBoth})
	if err != nil {
		return res, fmt.Errorf("listing open steps: %w", err)
	}
	byParent := make(map[string][]string)
	for _, s := range steps {
		if s.Status != "open" || strings.TrimSpace(s.Assignee) != "" || isGCExempt(s) {
			continue
		}
		parent := stepParentID(s)
		if _, ok := closedRoots[parent]; !ok {
			continue
		}
		byParent[parent] = append(byParent[parent], s.ID)
		res.candidates++
	}
	if !res.enforced || res.candidates == 0 {
		return res, nil
	}
	parents := make([]string, 0, len(byParent))
	for p := range byParent {
		parents = append(parents, p)
	}
	slices.Sort(parents)
	var ids []string
	for _, p := range parents {
		if batchCap > 0 && len(ids) >= batchCap {
			break
		}
		ids = append(ids, byParent[p]...)
	}
	ordered, err := closeorder.Order(store, ids)
	if err != nil {
		return res, fmt.Errorf("ordering leaked steps: %w", err)
	}
	res.closed, err = store.CloseAll(ordered, map[string]string{
		beadmeta.OutcomeMetadataKey: beadmeta.OutcomeSkipped,
		"close_reason":              leakedStepCloseReason,
	})
	if err != nil {
		return res, fmt.Errorf("closing leaked steps: %w", err)
	}
	return res, nil
}

// stepParentID returns a step's parent from its parent field, falling back to
// a parent-child dependency for projections that omit the field.
func stepParentID(b beads.Bead) string {
	if p := strings.TrimSpace(b.ParentID); p != "" {
		return p
	}
	for _, d := range b.Dependencies {
		if d.Type == "parent-child" {
			return strings.TrimSpace(d.DependsOnID)
		}
	}
	return ""
}

// sweepLeakedRigSteps runs the leaked-step sweep over every rig store. The
// city-store wisp GC never sees rig stores, so molecules living in a rig (for
// example crm's review molecules) had no convergence path at all.
func sweepLeakedRigSteps(ctx context.Context, rigStores map[string]beads.Store) error {
	names := make([]string, 0, len(rigStores))
	for name := range rigStores {
		names = append(names, name)
	}
	slices.Sort(names)
	var sweepErr error
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return errors.Join(sweepErr, fmt.Errorf("leaked-step sweep abandoned before rig %s: %w", name, err))
		}
		start := time.Now()
		res, err := closeLeakedMoleculeSteps(rigStores[name], wispGCLeakedStepBatchCap)
		log.Printf("wisp gc: leaked molecule steps rig=%s candidates=%d closed=%d enforce=%t took=%s",
			name, res.candidates, res.closed, res.enforced, time.Since(start).Round(time.Millisecond))
		if err != nil {
			sweepErr = errors.Join(sweepErr, fmt.Errorf("rig %s: %w", name, err))
		}
	}
	return sweepErr
}
