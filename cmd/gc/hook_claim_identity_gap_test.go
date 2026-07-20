package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestHookClaimIdentityGap_gci310k is the isolated reproduction of the nvf76
// claim-identity gap (gci-310k): a routed pool bead pinned to a now-DEAD
// sibling instance is claimable by NEITHER path for a fresh pool member —
//   - adopt (hookClaimExistingOrAssigned) requires an exact-string identity
//     match on the assignee, which the fresh instance's IdentityCandidates do
//     not contain; and
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
	if _, _, ok := hookClaimExistingOrAssigned([]beads.Bead{pinnedOpen}, hookClaimOptions{IdentityCandidates: freshCandidates}); ok {
		t.Fatalf("adopt path unexpectedly claimed a dead-sibling-pinned bead for a fresh instance")
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
	if _, _, ok := hookClaimExistingOrAssigned([]beads.Bead{ownInProgress}, hookClaimOptions{IdentityCandidates: freshCandidates}); !ok {
		t.Fatalf("adopt path should reclaim a bead pinned to the caller's own id")
	}

	// --- FIX DIRECTION: normalized identity candidates restore adoption ---
	// If the fresh instance's IdentityCandidates included the pool-scoped /
	// sibling id (the identity-normalization fix), it adopts the pinned bead.
	normalizedCandidates := hookClaimIdentityCandidates(freshInstance, deadSibling)
	if _, _, ok := hookClaimExistingOrAssigned([]beads.Bead{pinnedOpen}, hookClaimOptions{IdentityCandidates: normalizedCandidates}); !ok {
		t.Fatalf("with normalized identity candidates a fresh instance should adopt the pool's pinned bead")
	}
}
