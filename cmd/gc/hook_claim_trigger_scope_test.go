package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestHookClaimPrioritizesTriggerBead_gci_a8y is the RED->GREEN test for the
// pool-worker claim-scoping fix (gci-a8y): a session spawned FOR a specific bead
// carries GC_TRIGGER_WORK_BEAD_ID, but the unscoped claim grabs the head of the
// shared routed queue instead of its own trigger bead. With trigger scoping the
// worker must claim EXACTLY its trigger bead when that bead is a claimable
// routed candidate.
func TestHookClaimPrioritizesTriggerBead_gci_a8y(t *testing.T) {
	const pool = "crm/gastown.polecat"
	// The generic oldest-first head is a DIFFERENT bead than this worker's trigger.
	candidates := []beads.Bead{
		{ID: "crm-yeb98x", Status: "open", Metadata: map[string]string{"gc.routed_to": pool}},
		{ID: "crm-1g4vjm.4", Status: "open", Metadata: map[string]string{"gc.routed_to": pool}},
	}
	output, err := json.Marshal(candidates)
	if err != nil {
		t.Fatalf("marshal candidates: %v", err)
	}

	var claimed string
	ops := hookClaimOps{
		Runner: func(string, string) (string, error) { return string(output), nil },
		Claim: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			claimed = beadID
			return beads.Bead{ID: beadID, Assignee: assignee, Status: "in_progress"}, true, nil
		},
		DrainAck: func(io.Writer) error { return nil },
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", ".", hookClaimOptions{
		Assignee:           "gastown__polecat-gc2-abc",
		IdentityCandidates: []string{"gastown__polecat-gc2-abc"},
		RouteTargets:       []string{pool},
		TriggerBeadID:      "crm-1g4vjm.4",
		JSON:               true,
	}, ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim() = %d, want 0; stderr=%s", code, stderr.String())
	}
	if claimed != "crm-1g4vjm.4" {
		t.Fatalf("claimed %q, want trigger bead crm-1g4vjm.4 (unscoped claim grabbed the queue head instead)", claimed)
	}
}

// TestHookClaimTriggerBeadNoOpWhenAbsentOrEmpty guards that trigger scoping does
// not disturb the normal oldest-first claim when there is no trigger bead, or
// when the trigger bead is not among the claimable candidates (e.g. it was
// claimed by a peer between spawn and claim) — the worker still takes the head.
func TestHookClaimTriggerBeadNoOpWhenAbsentOrEmpty(t *testing.T) {
	const pool = "crm/gastown.polecat"
	candidates := []beads.Bead{
		{ID: "crm-yeb98x", Status: "open", Metadata: map[string]string{"gc.routed_to": pool}},
		{ID: "crm-lob3o0", Status: "open", Metadata: map[string]string{"gc.routed_to": pool}},
	}
	output, _ := json.Marshal(candidates)

	run := func(trigger string) string {
		var claimed string
		ops := hookClaimOps{
			Runner: func(string, string) (string, error) { return string(output), nil },
			Claim: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
				claimed = beadID
				return beads.Bead{ID: beadID, Assignee: assignee, Status: "in_progress"}, true, nil
			},
			DrainAck: func(io.Writer) error { return nil },
		}
		var stdout, stderr bytes.Buffer
		if code := doHookClaim("q", ".", hookClaimOptions{
			Assignee: "w", IdentityCandidates: []string{"w"}, RouteTargets: []string{pool},
			TriggerBeadID: trigger, JSON: true,
		}, ops, &stdout, &stderr); code != 0 {
			t.Fatalf("code=%d stderr=%s", code, stderr.String())
		}
		return claimed
	}

	if got := run(""); got != "crm-yeb98x" {
		t.Fatalf("no trigger: claimed %q, want oldest head crm-yeb98x", got)
	}
	if got := run("crm-not-in-queue"); got != "crm-yeb98x" {
		t.Fatalf("absent trigger: claimed %q, want oldest head crm-yeb98x", got)
	}
}
