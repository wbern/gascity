package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// TestReconcileSessionBeads_StrandedRepairVsPoolDemand_gci310k is the
// deterministic experiment for the nvf76/gci-310k deadlock hypothesis:
//
//	poolFreeable := !shouldWake && !alive && isPoolManaged && slotFreeable
//
// and shouldWake is set by demand-driven wake (compute_awake_set.go:400). So a
// dead pool instance that still holds stranded work should get that work
// released by repairStrandedPoolWorkerBead — UNLESS the pool also has pending
// demand, which flips shouldWake=true and forces poolFreeable=false, skipping
// the repair. That is the deadlock: the stranded work that DRIVES the demand is
// exactly what blocks the reaper that would free it, so fresh members keep
// hitting the nvf76 identity gap and the pool spawn-loops.
//
// The subtest with poolDesired==nil is the control (repair fires); the subtest
// with demand reproduces (or refutes) the block. It asserts nothing rigid yet —
// it logs the observed behavior so the run itself confirms which gate is live
// before any fix is written.
func TestReconcileSessionBeads_StrandedRepairVsPoolDemand_gci310k(t *testing.T) {
	runOnce := func(t *testing.T, poolDesired map[string]int) (assignee, status string) {
		t.Helper()
		env := newReconcilerTestEnv()
		env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
		env.addDesired("worker", "worker", false) // runtime NOT running — dead
		session := env.createSessionBead("worker", "worker")
		env.setSessionMetadata(&session, map[string]string{
			"state":                 "asleep",
			"sleep_reason":          "idle",
			poolManagedMetadataKey:  boolMetadata(true),
			strandedEventEmittedKey: env.clk.Now().Add(-strandedRepairConfirmGrace - time.Minute).Format(time.RFC3339),
		})
		work, err := env.store.Create(beads.Bead{
			Title: "stranded", Type: "task", Status: "open", Assignee: session.ID,
			Metadata: map[string]string{"gc.routed_to": "worker"},
		})
		if err != nil {
			t.Fatalf("create work: %v", err)
		}
		ip := "in_progress"
		if err := env.store.Update(work.ID, beads.UpdateOpts{Status: &ip}); err != nil {
			t.Fatalf("set in_progress: %v", err)
		}
		cur, _ := env.store.Get(session.ID)

		reconcileSessionBeadsAtPath(
			context.Background(), "", []beads.Bead{cur}, env.desiredState,
			map[string]bool{"worker": true}, env.cfg, env.sp, env.store,
			newFakeDrainOps(), nil, nil, nil, env.dt,
			poolDesired, // demand knob
			false, nil, "", nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
		)
		got, _ := env.store.Get(work.ID)
		return got.Assignee, got.Status
	}

	// Regression guard: confirmed-stranded pool work is reclaimed whether or not
	// the pool has pending demand. (This REFUTES the demand-deadlock hypothesis
	// for gci-310k: shouldWake does not block the stranded-work repair.)
	ctrlAssignee, ctrlStatus := runOnce(t, nil)
	if ctrlAssignee != "" || ctrlStatus != "open" {
		t.Fatalf("no-demand: stranded work must be reclaimed; got assignee=%q status=%q", ctrlAssignee, ctrlStatus)
	}
	demandAssignee, demandStatus := runOnce(t, map[string]int{"worker": 1})
	if demandAssignee != "" || demandStatus != "open" {
		t.Fatalf("with-demand: stranded work must STILL be reclaimed (demand must not block repair); got assignee=%q status=%q", demandAssignee, demandStatus)
	}
}
