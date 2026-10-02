package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// fakeAssignedStepStore models one session's continuation group: steps
// preassigned to the session and left open, some blocked by an open sibling.
type fakeAssignedStepStore struct {
	t       *testing.T
	beads   map[string]beads.Bead
	claims  []string
	blocked map[string]bool
}

func newFakeAssignedStepStore(t *testing.T, session, route string, ready string, blocked ...string) *fakeAssignedStepStore {
	t.Helper()
	s := &fakeAssignedStepStore{t: t, beads: map[string]beads.Bead{}, blocked: map[string]bool{}}
	meta := func() map[string]string {
		return map[string]string{beadmeta.RoutedToMetadataKey: route, "gc.root_bead_id": "root-1"}
	}
	s.beads[ready] = beads.Bead{ID: ready, Status: "open", Assignee: session, Metadata: meta()}
	for _, id := range blocked {
		s.blocked[id] = true
		s.beads[id] = beads.Bead{
			ID: id, Status: "open", Assignee: session, Metadata: meta(),
			Dependencies: []beads.Dep{{IssueID: id, DependsOnID: ready, Type: "blocks", Status: "open"}},
		}
	}
	return s
}

func (s *fakeAssignedStepStore) ops() hookClaimOps {
	return hookClaimOps{
		// The work query is bd ready: it returns only ready beads.
		Runner: func(string, string) (string, error) {
			var ready []beads.Bead
			for _, b := range s.beads {
				if b.Status == "open" && !s.blocked[b.ID] {
					ready = append(ready, b)
				}
			}
			out, err := json.Marshal(ready)
			return string(out), err
		},
		ResolveBead: func(_ context.Context, _ string, _ []string, id string) (beads.Bead, bool, error) {
			b, ok := s.beads[id]
			return b, ok, nil
		},
		Claim: func(_ context.Context, _ string, _ []string, id, actor string) (beads.Bead, bool, error) {
			if s.blocked[id] {
				s.t.Errorf("claimed blocked step %s", id)
			}
			s.claims = append(s.claims, id)
			b := s.beads[id]
			if b.Assignee != actor {
				return b, false, nil
			}
			b.Status = "in_progress"
			s.beads[id] = b
			return b, true, nil
		},
		EmitClaimRejected: func(string, string, string) {},
		ResolveWorkBranch: func(string) string { return "" },
	}
}

// REGRESSION (GC3 2026-10-02, gci-tyuulz): the reconciler pointed the session's
// trigger at a BLOCKED step it had preassigned, and the trigger path returned
// that step as ready_assignment without claiming it. The agent was handed work
// it could not start, the actually-ready step stayed open, and both pool slots
// idled on owned work for days.
func TestDoHookClaimOwnBlockedTriggerClaimsTheReadyAssignedStep(t *testing.T) {
	const session = "pool__codex-polecat-gc2-srrmkg"
	const route = "gas-city-infra/pool.codex-polecat"
	store := newFakeAssignedStepStore(t, session, route, "gci-h2wu3d", "gci-9966qc", "gci-9xz1lp", "gci-qb7llj")
	opts := hookClaimOptions{
		Assignee:           session,
		IdentityCandidates: []string{session},
		RouteTargets:       []string{route},
		TriggerBeadID:      "gci-qb7llj",
		JSON:               true,
	}

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", opts, store.ops(), &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\nraw: %s", err, stdout.String())
	}
	if result.Action != "work" || result.BeadID != "gci-h2wu3d" {
		t.Fatalf("result = %+v, want work on the ready step gci-h2wu3d; stderr=%s", result, stderr.String())
	}
	if got := store.beads["gci-h2wu3d"].Status; got != "in_progress" {
		t.Fatalf("ready step status = %q, want in_progress (claimed, not just reported)", got)
	}
	for id := range store.blocked {
		if store.beads[id].Status != "open" {
			t.Fatalf("blocked step %s status = %q, want untouched open", id, store.beads[id].Status)
		}
	}
	if len(store.claims) != 1 || store.claims[0] != "gci-h2wu3d" {
		t.Fatalf("claims = %v, want exactly [gci-h2wu3d]", store.claims)
	}
}

// An own-assigned open trigger that is ready is promoted through the store's
// idempotent claim, exactly as the work-query path does (upstream #4835).
func TestDoHookTriggerClaimPromotesOwnReadyOpenTrigger(t *testing.T) {
	const session = "worker-gc2-abc"
	store := newFakeAssignedStepStore(t, session, "rig/pool.worker", "step-ready")
	ops := store.ops()
	ops.Runner = func(string, string) (string, error) {
		t.Fatal("work_query must not run when the trigger itself is claimable")
		return "", nil
	}

	var stdout, stderr bytes.Buffer
	res := doHookTriggerClaim("step-ready", "city", hookClaimOptions{
		Assignee: session, IdentityCandidates: []string{session}, RouteTargets: []string{"rig/pool.worker"}, JSON: true,
	}, ops, &stdout, &stderr)
	if !res.terminal || res.code != 0 {
		t.Fatalf("doHookTriggerClaim = %+v, want successful terminal; stderr=%s", res, stderr.String())
	}
	if len(store.claims) != 1 || store.claims[0] != "step-ready" {
		t.Fatalf("claims = %v, want exactly [step-ready]", store.claims)
	}
	if got := store.beads["step-ready"].Status; got != "in_progress" {
		t.Fatalf("trigger status = %q, want in_progress", got)
	}
	var got hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\nraw: %s", err, stdout.String())
	}
	if got.Action != "work" || got.Reason != "ready_assignment" || got.BeadID != "step-ready" || got.Assignee != session {
		t.Fatalf("result = %+v, want work/ready_assignment on step-ready", got)
	}
}

// An own-assigned open trigger that is blocked is not work: the trigger path
// falls through to the route-scoped pool query without claiming or printing.
func TestDoHookTriggerClaimOwnBlockedOpenTriggerFallsThrough(t *testing.T) {
	const session = "worker-gc2-abc"
	store := newFakeAssignedStepStore(t, session, "rig/pool.worker", "step-ready", "step-blocked")

	var stdout, stderr bytes.Buffer
	res := doHookTriggerClaim("step-blocked", "city", hookClaimOptions{
		Assignee: session, IdentityCandidates: []string{session}, RouteTargets: []string{"rig/pool.worker"}, JSON: true,
	}, store.ops(), &stdout, &stderr)
	if res.terminal {
		t.Fatalf("doHookTriggerClaim = %+v, want non-terminal fall-through for a blocked trigger", res)
	}
	if len(store.claims) != 0 {
		t.Fatalf("claims = %v, want none for a blocked trigger", store.claims)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want nothing written on fall-through", stdout.String())
	}
	if !strings.Contains(stderr.String(), "step-blocked") || !strings.Contains(stderr.String(), "blocked by step-ready") {
		t.Fatalf("stderr = %q, want the blocked reason for the trigger", stderr.String())
	}
}

// A store failure while promoting this session's own trigger fails closed:
// ownership is unresolved, so the session must not go claim unrelated work.
func TestDoHookTriggerClaimOwnOpenTriggerClaimErrorFailsClosed(t *testing.T) {
	const session = "worker-gc2-abc"
	store := newFakeAssignedStepStore(t, session, "rig/pool.worker", "step-ready")
	ops := store.ops()
	ops.Claim = func(context.Context, string, []string, string, string) (beads.Bead, bool, error) {
		return beads.Bead{}, false, errors.New("dolt unavailable")
	}

	var stdout, stderr bytes.Buffer
	res := doHookTriggerClaim("step-ready", "city", hookClaimOptions{
		Assignee: session, IdentityCandidates: []string{session}, RouteTargets: []string{"rig/pool.worker"}, JSON: true,
	}, ops, &stdout, &stderr)
	if !res.terminal || res.code != 1 {
		t.Fatalf("doHookTriggerClaim = %+v, want terminal failure code 1", res)
	}
	if !strings.Contains(stderr.String(), "dolt unavailable") {
		t.Fatalf("stderr = %q, want the claim error", stderr.String())
	}
}
