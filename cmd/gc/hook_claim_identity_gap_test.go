package main

import (
	"bytes"
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestHookClaimIdentityGap_gci310k is the isolated reproduction of the nvf76
// claim-identity gap (gci-310k): a routed pool bead pinned to a now-DEAD
// sibling instance is claimable by NEITHER path for a fresh pool member —
//   - adopt (hookClaimExistingAssignment for an in_progress bead,
//     claimFirstReadyHookAssignment for an open one) requires an identity match
//     on the assignee, which the fresh instance's IdentityCandidates do not
//     contain; and
//   - fresh-claim (hookCandidateClaimable) requires assignee == "".
//
// So the fresh member gets no_work and the pool spawn-loops. The controls pin
// route+readiness as NOT the blocker, and the fix-direction case shows that
// widening IdentityCandidates to the pool-scoped/normalized id restores
// adoption — i.e. identity normalization is the fix.
func TestHookClaimIdentityGap_gci310k(t *testing.T) {
	const (
		pool          = "crm/gastown.polecat"
		deadSibling   = "crm/gastown.polecat-gc2-pttkk" // prior pool member, now dead
		freshInstance = "crm/gastown.polecat-gc2-o2iiz" // current pool member
	)
	routeTargets := hookClaimRouteTargets(pool)
	freshCandidates := hookClaimIdentityCandidates(freshInstance)

	// The live shape: crm-1g4vjm.7, open, routed to the pool, assignee pinned to
	// the dead sibling instance.
	pinnedOpen := beads.Bead{
		ID:       "crm-1g4vjm.7",
		Status:   "open",
		Assignee: deadSibling,
		Metadata: map[string]string{"gc.routed_to": pool},
	}

	// --- THE BUG: fresh instance can claim it via NEITHER path ---
	if _, _, ok := hookClaimExistingAssignment([]beads.Bead{pinnedOpen}, hookClaimOptions{IdentityCandidates: freshCandidates}); ok {
		t.Fatalf("adopt path unexpectedly claimed a dead-sibling-pinned bead for a fresh instance")
	}
	// The open-and-mine half of adoption lives in claimFirstReadyHookAssignment,
	// which must likewise decline: the assignee is not one of this instance's
	// identities, so the bead is never even offered to the claim mutation.
	var bugStdout, bugStderr bytes.Buffer
	bugOps := hookClaimOps{
		Claim: func(context.Context, string, []string, string, string) (beads.Bead, bool, error) {
			t.Fatal("ready-assignment path must not claim a bead pinned to a foreign identity")
			return beads.Bead{}, false, nil
		},
	}
	if got := claimFirstReadyHookAssignment(
		[]beads.Bead{pinnedOpen},
		hookClaimOptions{IdentityCandidates: freshCandidates, RouteTargets: routeTargets},
		bugOps, "/tmp/work", &bugStdout, &bugStderr,
	); got.terminal {
		t.Fatalf("ready-assignment path unexpectedly claimed a dead-sibling-pinned bead: %+v", got)
	}
	if hookCandidateClaimable(pinnedOpen, routeTargets) {
		t.Fatalf("fresh-claim path unexpectedly eligible for an already-assigned bead")
	}
	// => neither path claims it: this is the no_work → respawn spawn-loop (gci-310k).

	// --- CONTROL 1: the SAME bead unassigned is freshly claimable ---
	// Proves route + readiness are fine; identity pinning is the sole blocker.
	unassigned := pinnedOpen
	unassigned.Assignee = ""
	if !hookCandidateClaimable(unassigned, routeTargets) {
		t.Fatalf("unassigned routed bead should be freshly claimable; route is not the blocker")
	}

	// --- CONTROL 2: a bead pinned to the caller's OWN id IS adoptable ---
	// Proves the adopt path works for an exact identity match (so the gap is
	// specifically the fresh-id-vs-dead-sibling mismatch, not a broken adopt).
	ownInProgress := pinnedOpen
	ownInProgress.Status = "in_progress"
	ownInProgress.Assignee = freshInstance
	if _, _, ok := hookClaimExistingAssignment([]beads.Bead{ownInProgress}, hookClaimOptions{IdentityCandidates: freshCandidates}); !ok {
		t.Fatalf("adopt path should reclaim a bead pinned to the caller's own id")
	}

	// --- FIX DIRECTION: normalized identity candidates restore adoption ---
	// If the fresh instance's IdentityCandidates included the pool-scoped /
	// sibling id (the identity-normalization fix), it adopts the pinned bead.
	// The bead is open, so post-#4835 adoption runs through
	// claimFirstReadyHookAssignment, which promotes it with the store's
	// idempotent claim mutation rather than reporting it workable in place.
	normalizedCandidates := hookClaimIdentityCandidates(freshInstance, deadSibling)
	var claimedID, claimActor string
	var fixStdout, fixStderr bytes.Buffer
	fixOps := hookClaimOps{
		Claim: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			claimedID, claimActor = beadID, assignee
			return beads.Bead{
				ID:       beadID,
				Status:   "in_progress",
				Assignee: assignee,
				Metadata: map[string]string{"gc.routed_to": pool},
			}, true, nil
		},
		ListContinuation: func(context.Context, string, []string, string, string) ([]beads.Bead, error) {
			return nil, nil
		},
		// Stubbed so the claim-time identity patch is empty and the work-result
		// write stays a pure in-memory path: no git shell-out, no store write.
		ResolveWorkBranch: func(string) string { return "" },
	}
	got := claimFirstReadyHookAssignment(
		[]beads.Bead{pinnedOpen},
		hookClaimOptions{IdentityCandidates: normalizedCandidates, RouteTargets: routeTargets},
		fixOps, "/tmp/work", &fixStdout, &fixStderr,
	)
	if !got.terminal || got.code != 0 {
		t.Fatalf("with normalized identity candidates a fresh instance should adopt the pool's pinned bead; got %+v stderr=%s", got, fixStderr.String())
	}
	if claimedID != pinnedOpen.ID {
		t.Fatalf("claimed bead = %q, want %q", claimedID, pinnedOpen.ID)
	}
	// bd's idempotent --claim path requires the actor to match the existing
	// assignee exactly, so adoption claims AS the dead sibling's id.
	if claimActor != deadSibling {
		t.Fatalf("claim actor = %q, want the bead's existing assignee %q", claimActor, deadSibling)
	}
}
