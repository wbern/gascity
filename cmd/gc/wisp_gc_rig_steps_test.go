package main

import (
	"bytes"
	"log"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

func setLeakedStepsEnforced(t *testing.T, enforced bool) {
	t.Helper()
	prev := closeLeakedStepsEnforced
	closeLeakedStepsEnforced = func() bool { return enforced }
	t.Cleanup(func() { closeLeakedStepsEnforced = prev })
}

// leakedStepsFixture is the GC3 crm shape (2026-10-02): a review molecule
// closed root-only by pr-review-patrol.sh, its steps left open, next to beads
// that must never be touched.
func leakedStepsFixture(now time.Time) *beads.MemStore {
	old := now.Add(-48 * time.Hour)
	bead := func(id, status, typ, parent string) beads.Bead {
		return beads.Bead{ID: id, Status: status, Type: typ, ParentID: parent, CreatedAt: old}
	}
	exempt := bead("mol-done.exempt", "open", "step", "mol-done")
	exempt.Metadata = map[string]string{beadmeta.GCExemptMetadataKey: "true"}
	assigned := bead("mol-done.assigned", "open", "step", "mol-done")
	assigned.Assignee = "reviewer-gc2-abc"
	return beads.NewMemStoreFrom(0, []beads.Bead{
		bead("mol-done", "closed", "molecule", ""),
		bead("mol-done.leaked-1", "open", "step", "mol-done"), // close
		bead("mol-done.leaked-2", "open", "step", "mol-done"), // close
		bead("mol-done.working", "in_progress", "step", "mol-done"),
		bead("mol-done.finished", "closed", "step", "mol-done"),
		bead("mol-done.task", "open", "task", "mol-done"), // real work, not a step
		assigned,
		exempt,
		bead("mol-live", "open", "molecule", ""),
		bead("mol-live.step", "open", "step", "mol-live"),
		bead("orphan.step", "open", "step", "missing-root"),
		bead("mol-gone-task", "closed", "task", ""),
		bead("mol-gone-task.step", "open", "step", "mol-gone-task"), // parent is not a molecule
	}, nil)
}

func statusOf(t *testing.T, store beads.Store, id string) string {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return b.Status
}

var leakedStepsUntouched = []string{
	"mol-done.working", "mol-done.task", "mol-done.assigned", "mol-done.exempt",
	"mol-live.step", "orphan.step", "mol-gone-task.step",
}

func TestCloseLeakedMoleculeSteps_DryRunCountsWithoutMutating(t *testing.T) {
	setLeakedStepsEnforced(t, false)
	store := leakedStepsFixture(time.Now())

	res, err := closeLeakedMoleculeSteps(store, 100)
	if err != nil {
		t.Fatalf("closeLeakedMoleculeSteps: %v", err)
	}
	if res.candidates != 2 || res.closed != 0 || res.enforced {
		t.Fatalf("result = %+v, want 2 candidates, 0 closed, dry-run", res)
	}
	for _, id := range append([]string{"mol-done.leaked-1", "mol-done.leaked-2"}, leakedStepsUntouched...) {
		if got := statusOf(t, store, id); got == "closed" {
			t.Fatalf("dry-run closed %s", id)
		}
	}
}

func TestCloseLeakedMoleculeSteps_EnforcedClosesOnlyOpenUnassignedStepsOfClosedRoots(t *testing.T) {
	setLeakedStepsEnforced(t, true)
	store := leakedStepsFixture(time.Now())

	res, err := closeLeakedMoleculeSteps(store, 100)
	if err != nil {
		t.Fatalf("closeLeakedMoleculeSteps: %v", err)
	}
	if res.candidates != 2 || res.closed != 2 || !res.enforced {
		t.Fatalf("result = %+v, want 2 candidates, 2 closed, enforced", res)
	}
	for _, id := range []string{"mol-done.leaked-1", "mol-done.leaked-2"} {
		b, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if b.Status != "closed" {
			t.Fatalf("%s status = %q, want closed", id, b.Status)
		}
		if b.Metadata["close_reason"] != leakedStepCloseReason || b.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomeSkipped {
			t.Fatalf("%s metadata = %v, want close_reason %q and outcome skipped", id, b.Metadata, leakedStepCloseReason)
		}
	}
	for _, id := range leakedStepsUntouched {
		if got := statusOf(t, store, id); got == "closed" {
			t.Fatalf("closed %s, which must stay untouched", id)
		}
	}
}

func TestCloseLeakedMoleculeSteps_BatchCapStopsAtMoleculeBoundaries(t *testing.T) {
	setLeakedStepsEnforced(t, true)
	bead := func(id, status, typ, parent string) beads.Bead {
		return beads.Bead{ID: id, Status: status, Type: typ, ParentID: parent, CreatedAt: time.Now().Add(-time.Hour)}
	}
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		bead("mol-a", "closed", "molecule", ""),
		bead("mol-a.1", "open", "step", "mol-a"),
		bead("mol-a.2", "open", "step", "mol-a"),
		bead("mol-b", "closed", "molecule", ""),
		bead("mol-b.1", "open", "step", "mol-b"),
	}, nil)

	// Cap 1 still closes all of mol-a: a molecule's steps are never split
	// across sweeps, so the ordered close sees every sibling blocker.
	res, err := closeLeakedMoleculeSteps(store, 1)
	if err != nil {
		t.Fatalf("closeLeakedMoleculeSteps: %v", err)
	}
	if res.candidates != 3 || res.closed != 2 {
		t.Fatalf("result = %+v, want 3 candidates and mol-a's 2 steps closed", res)
	}
	if statusOf(t, store, "mol-b.1") != "open" {
		t.Fatal("mol-b.1 closed in the first sweep, want it left for the next one")
	}
	res, err = closeLeakedMoleculeSteps(store, 1)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if res.candidates != 1 || res.closed != 1 || statusOf(t, store, "mol-b.1") != "closed" {
		t.Fatalf("second sweep = %+v, want the remaining mol-b.1 closed", res)
	}
}

// The sweep covers every rig store, which the city-store wisp GC never sees,
// and logs one line per rig so the rollout can be measured.
func TestCityRuntimeWispGCSweepClosesLeakedStepsInRigStores(t *testing.T) {
	setLeakedStepsEnforced(t, true)
	var logBuf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&logBuf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })

	rig := leakedStepsFixture(time.Now())
	var stdout, stderr bytes.Buffer
	cr := newWispGCTestRuntime(t, onceWispGC{fixedWispGC: fixedWispGC{}, ran: &atomic.Bool{}}, &stdout, &stderr)
	cr.standaloneRigStores = map[string]beads.Store{"crm": rig}

	runTestTick(cr)
	if !cr.wispSweeps.waitIdle(5 * time.Second) {
		t.Fatal("sweep did not finish")
	}
	for _, id := range []string{"mol-done.leaked-1", "mol-done.leaked-2"} {
		if got := statusOf(t, rig, id); got != "closed" {
			t.Fatalf("rig step %s status = %q, want closed by the sweep", id, got)
		}
	}
	if !strings.Contains(logBuf.String(), "wisp gc: leaked molecule steps rig=crm candidates=2 closed=2 enforce=true") {
		t.Fatalf("log = %q, want the per-rig leaked-steps line", logBuf.String())
	}
}
