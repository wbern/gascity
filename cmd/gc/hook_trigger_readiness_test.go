package main

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// bdShowGraphV2SelfReview is the EXACT wire shape `bd show --json` emits, taken
// from the live crm store on GC3 2026-08-12. Two things about it drive every
// test in this file:
//
//   - there is no top-level "blocked_by" and no "is_blocked". The readiness
//     signal the generic discovery path filters on simply does not exist here.
//   - each entry of "dependencies" is a FULL BEAD OBJECT keyed id/status/
//     dependency_type — not the {issue_id, depends_on_id, type} relational form
//     that beads.Dep declares.
//
// That mismatch is why the trigger path had no dependency check: it could not
// see one through the typed struct even if it looked.
const bdShowGraphV2SelfReview = `{
  "id": "crm-q6zfr9",
  "title": "mol-polecat-report.self-review",
  "status": "open",
  "issue_type": "task",
  "assignee": "",
  "metadata": {"gc.routed_to": "crm/gastown.polecat"},
  "dependencies": [
    {"id": "crm-e9jahp", "title": "Investigate", "status": "open", "dependency_type": "blocks"}
  ]
}`

func TestDepDecodesBdShowDependencyWireShape(t *testing.T) {
	// The blocker's identity and status must survive the decode. Before the fix
	// every field came back empty: the JSON keys id/status/dependency_type match
	// none of Dep's tags (issue_id/depends_on_id/type), so encoding/json filled
	// in nothing and callers saw len(Dependencies)==1 with no usable content.
	var bead beads.Bead
	if err := json.Unmarshal([]byte(bdShowGraphV2SelfReview), &bead); err != nil {
		t.Fatalf("unmarshal bd show payload: %v", err)
	}
	if len(bead.Dependencies) != 1 {
		t.Fatalf("Dependencies = %#v, want exactly one blocker", bead.Dependencies)
	}
	dep := bead.Dependencies[0]
	if dep.DependsOnID != "crm-e9jahp" {
		t.Errorf("DependsOnID = %q, want crm-e9jahp", dep.DependsOnID)
	}
	if dep.Status != "open" {
		t.Errorf("Status = %q, want open", dep.Status)
	}
	if dep.Type != "blocks" {
		t.Errorf("Type = %q, want blocks", dep.Type)
	}
}

func TestDepStillDecodesRelationalWireShape(t *testing.T) {
	// The relational form is what the native store and the caching store speak.
	// Teaching Dep the bd-show shape must not cost us this one.
	var dep beads.Dep
	if err := json.Unmarshal([]byte(`{"issue_id":"a-1","depends_on_id":"b-2","type":"blocks"}`), &dep); err != nil {
		t.Fatalf("unmarshal relational dep: %v", err)
	}
	if dep.IssueID != "a-1" || dep.DependsOnID != "b-2" || dep.Type != "blocks" {
		t.Fatalf("dep = %#v, want issue_id/depends_on_id/type preserved", dep)
	}
}

func TestDepRelationalMarshalIsUnchanged(t *testing.T) {
	// Status is decode-only for the bd-show projection; an empty one must not
	// start appearing on the wire where the stores round-trip Dep.
	out, err := json.Marshal(beads.Dep{IssueID: "a-1", DependsOnID: "b-2", Type: "blocks"})
	if err != nil {
		t.Fatalf("marshal dep: %v", err)
	}
	if got, want := string(out), `{"issue_id":"a-1","depends_on_id":"b-2","type":"blocks"}`; got != want {
		t.Fatalf("marshal = %s, want %s", got, want)
	}
}

// hookTriggerReadinessOps builds the stub ops shared by the claim tests below.
// resolved is the bead the trigger id resolves to; ready is the raw work-query
// output for the route-scoped pool.
func hookTriggerReadinessOps(t *testing.T, resolved beads.Bead, ready string, claims *[]string) hookClaimOps {
	t.Helper()
	return hookClaimOps{
		Runner: func(string, string) (string, error) { return ready, nil },
		ResolveBead: func(_ context.Context, _ string, _ []string, _ string) (beads.Bead, bool, error) {
			return resolved, true, nil
		},
		Claim: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			*claims = append(*claims, beadID)
			return beads.Bead{
				ID:       beadID,
				Status:   "in_progress",
				Assignee: assignee,
				Metadata: map[string]string{"gc.routed_to": "crm/gastown.polecat"},
			}, true, nil
		},
	}
}

func hookTriggerReadinessOpts() hookClaimOptions {
	return hookClaimOptions{
		Assignee:           "gastown__polecat-gc2-7fzr2",
		IdentityCandidates: []string{"gastown__polecat-gc2-7fzr2"},
		RouteTargets:       []string{"crm/gastown.polecat"},
		TriggerBeadID:      "crm-q6zfr9",
		JSON:               true,
	}
}

func TestDoHookClaimTriggerWithOpenBlockerFallsThroughToReadyPoolWork(t *testing.T) {
	// LIVE REPRODUCTION, GC3 2026-08-12T00:25Z (gci-pha reopen). Replacement nux
	// session gc2-7fzr2 claimed crm-q6zfr9 (mol-polecat-report.self-review)
	// through the TRIGGER path while its blocker crm-e9jahp was still open. The
	// store then refused the close: "cannot close crm-q6zfr9: blocked by open
	// issues [crm-e9jahp]". Selection said yes, the close gate said no.
	//
	// The generic discovery path would never have picked it — it drops
	// dep-blocked candidates. The trigger path ran four checks (exists,
	// already-mine, unassigned+open, route match) and none of them looked at
	// dependencies.
	var resolved beads.Bead
	if err := json.Unmarshal([]byte(bdShowGraphV2SelfReview), &resolved); err != nil {
		t.Fatalf("unmarshal trigger bead: %v", err)
	}
	ready := `[{"id":"ready-routed","status":"open","metadata":{"gc.routed_to":"crm/gastown.polecat"}}]`

	var claims []string
	ops := hookTriggerReadinessOps(t, resolved, ready, &claims)
	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", hookTriggerReadinessOpts(), ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	for _, claimed := range claims {
		if claimed == "crm-q6zfr9" {
			t.Fatalf("claimed dep-blocked trigger crm-q6zfr9; claims=%#v stderr=%s", claims, stderr.String())
		}
	}
	// Liveness must survive the new gate: refusing the trigger has to leave the
	// session on route-scoped pool work, not drain it.
	if len(claims) != 1 || claims[0] != "ready-routed" {
		t.Fatalf("claims = %#v, want only ready-routed", claims)
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\nraw: %s", err, stdout.String())
	}
	if result.Action != "work" || result.BeadID != "ready-routed" {
		t.Fatalf("result = %+v, want work on ready-routed", result)
	}
}

func TestDoHookClaimTriggerWithClosedBlockerIsStillClaimed(t *testing.T) {
	// The other half of correctness, and the reason a blunt
	// len(Dependencies) > 0 gate is wrong: a satisfied dependency must not park
	// its successor. This is the over-filtering regression guard.
	resolved := beads.Bead{
		ID:       "crm-q6zfr9",
		Status:   "open",
		Metadata: map[string]string{"gc.routed_to": "crm/gastown.polecat"},
		Dependencies: []beads.Dep{
			{DependsOnID: "crm-e9jahp", Status: "closed", Type: "blocks"},
		},
	}
	ready := `[{"id":"crm-q6zfr9","status":"open","metadata":{"gc.routed_to":"crm/gastown.polecat"}}]`

	var claims []string
	ops := hookTriggerReadinessOps(t, resolved, ready, &claims)
	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", hookTriggerReadinessOpts(), ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	if len(claims) != 1 || claims[0] != "crm-q6zfr9" {
		t.Fatalf("claims = %#v, want the trigger claimed once (closed blockers do not park work)", claims)
	}
}

func TestDoHookClaimTriggerStrippedByReadinessFilterFallsThrough(t *testing.T) {
	// Belt and braces for wire-shape drift, in the ONLY form that carries signal.
	// The resolved trigger looks perfect on its own fields — open, unassigned,
	// route-matched, no dependencies visible — because `bd show` emits neither
	// blocked_by nor is_blocked. The work query DID surface this bead, and the
	// readiness filter dropped it on the blocked_by the projection does carry.
	// The projection therefore knows something the typed bead cannot show, so the
	// trigger path must defer to it rather than claim.
	resolved := beads.Bead{
		ID:       "crm-q6zfr9",
		Status:   "open",
		Metadata: map[string]string{"gc.routed_to": "crm/gastown.polecat"},
	}
	ready := `[
		{"id":"crm-q6zfr9","status":"open","blocked_by":["crm-e9jahp"],"metadata":{"gc.routed_to":"crm/gastown.polecat"}},
		{"id":"ready-routed","status":"open","metadata":{"gc.routed_to":"crm/gastown.polecat"}}
	]`

	var claims []string
	ops := hookTriggerReadinessOps(t, resolved, ready, &claims)
	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", hookTriggerReadinessOpts(), ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	if len(claims) != 1 || claims[0] != "ready-routed" {
		t.Fatalf("claims = %#v, want only ready-routed", claims)
	}
}

func TestDoHookClaimTriggerAbsentFromWorkQueryIsStillClaimed(t *testing.T) {
	// The counterweight, and the reason the check above is narrowed to
	// seen-then-stripped. The work query is NOT guaranteed to cover the trigger's
	// route — here it returns an unrelated, unrouted global candidate — and the
	// trigger path exists to claim work the pool query never surfaced. Treating
	// absence as unready would refuse legitimate triggers, which is the false
	// refusal TestDoHookClaimTargetsTriggerBeadOverUnrelatedWorkQueryResult
	// caught when this guard was first written the naive way.
	resolved := beads.Bead{
		ID:       "crm-q6zfr9",
		Status:   "open",
		Metadata: map[string]string{"gc.routed_to": "crm/gastown.polecat"},
	}
	ready := `[{"id":"unrelated-global","status":"open","priority":1,"metadata":{}}]`

	var claims []string
	ops := hookTriggerReadinessOps(t, resolved, ready, &claims)
	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", hookTriggerReadinessOpts(), ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	if len(claims) != 1 || claims[0] != "crm-q6zfr9" {
		t.Fatalf("claims = %#v, want the trigger claimed despite being absent from the work query", claims)
	}
}

func TestDoHookClaimTriggerClaimedWhenWorkQueryUnusable(t *testing.T) {
	// The membership check must fail OPEN. A broken or empty work query is a
	// real GC3 condition — the 2026-08-12 incident itself recorded "missing work
	// query runner" — and it must not become a second way to starve a worker of
	// its own trigger. With no usable projection we fall back to the typed
	// predicate alone, which sees no blockers here, so the trigger is claimed.
	resolved := beads.Bead{
		ID:       "crm-q6zfr9",
		Status:   "open",
		Metadata: map[string]string{"gc.routed_to": "crm/gastown.polecat"},
	}

	var claims []string
	ops := hookTriggerReadinessOps(t, resolved, "", &claims)
	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", hookTriggerReadinessOpts(), ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	if len(claims) != 1 || claims[0] != "crm-q6zfr9" {
		t.Fatalf("claims = %#v, want the trigger claimed when the projection is unusable", claims)
	}
}

func TestDoHookClaimTriggerSelfBlockedStatusFallsThrough(t *testing.T) {
	// status=blocked on the trigger itself already falls through today (the
	// PARKED-trigger fix, c31314f58). Pinning it here so the readiness rework
	// cannot regress it back into a drain.
	resolved := beads.Bead{
		ID:       "crm-907kq8",
		Status:   "blocked",
		Metadata: map[string]string{"gc.routed_to": "crm/gastown.polecat"},
	}
	ready := `[{"id":"ready-routed","status":"open","metadata":{"gc.routed_to":"crm/gastown.polecat"}}]`

	var claims []string
	ops := hookTriggerReadinessOps(t, resolved, ready, &claims)
	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", hookTriggerReadinessOpts(), ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	if len(claims) != 1 || claims[0] != "ready-routed" {
		t.Fatalf("claims = %#v, want only ready-routed", claims)
	}
}
