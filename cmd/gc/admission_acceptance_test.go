package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// TestAdmissionGate_AllowPreservesFullDemand verifies criterion 1:
// With a boolean gate returning allow (exit 0), pool demand equals the expressed
// demand (e.g. 5), NOT capped to 1.
func TestAdmissionGate_AllowPreservesFullDemand(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("worker", "", intPtr(10), 0)},
	}
	verdicts := map[string]AdmissionVerdict{
		"worker": {
			State:       AdmissionAllow,
			Allowed:     true,
			Reason:      "",
			EvaluatedAt: time.Now(),
		},
	}
	counts := PoolDesiredCounts(ComputePoolDesiredStatesWithAdmission(
		cfg,
		nil,
		nil,
		map[string]int{"worker": 5},
		verdicts,
	))

	if got := counts["worker"]; got != 5 {
		t.Fatalf("worker desired = %d, want 5 (admission gate allow must preserve expressed demand, not clamp to 1)", got)
	}
}

// TestAdmissionGate_DenyZerosNewDemandAndRecordsTrace verifies criterion 2:
// With the gate returning deny (exit 1), demand is 0 and the veto reason is recorded in trace.
func TestAdmissionGate_DenyZerosNewDemandAndRecordsTrace(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("worker", "", intPtr(10), 0)},
	}
	verdicts := map[string]AdmissionVerdict{
		"worker": {
			State:       AdmissionDeny,
			Allowed:     false,
			Reason:      "cooling down",
			EvaluatedAt: time.Now(),
		},
	}
	trace := newPoolDesiredStateTestTrace("worker")
	result := computePoolDesiredStates(
		cfg,
		nil,
		nil,
		nil,
		map[string]int{"worker": 5},
		nil,
		0,
		poolNewDemandLoadVeto{},
		verdicts,
		trace,
	)
	counts := PoolDesiredCounts(result)

	if got := counts["worker"]; got != 0 {
		t.Fatalf("worker desired = %d, want 0 when admission gate denies demand", got)
	}

	if got := trace.decisionCounts[string(TraceSiteAdmissionCheckExec)]; got != 1 {
		t.Fatalf("admission decision trace count = %d, want 1; records=%#v", got, trace.records)
	}
	rec := poolTraceDecision(t, trace, TraceSiteAdmissionCheckExec)
	if rec.ReasonCode != TraceReasonAdmissionGate {
		t.Fatalf("trace reason = %q, want %q", rec.ReasonCode, TraceReasonAdmissionGate)
	}
	if rec.OutcomeCode != TraceOutcomeDeny {
		t.Fatalf("trace outcome = %q, want %q", rec.OutcomeCode, TraceOutcomeDeny)
	}
	if got := poolTraceFieldString(t, rec.Fields, "reason"); got != "cooling down" {
		t.Fatalf("trace veto reason = %q, want 'cooling down'", got)
	}
	if got := poolTraceFieldInt(t, rec.Fields, "demand_vetoed"); got != 5 {
		t.Fatalf("trace demand_vetoed = %d, want 5", got)
	}
	if got := poolTraceFieldInt(t, rec.Fields, "in_flight_retained"); got != 0 {
		t.Fatalf("trace in_flight_retained = %d, want 0", got)
	}
}

// TestAdmissionGate_DenyRetainsInFlightSessions verifies criterion 5 (in-flight retention):
// In-flight requests bypass the gate so already-started sessions are not drained as orphans.
func TestAdmissionGate_DenyRetainsInFlightSessions(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "", intPtr(10), 0)},
	}
	sessions := []beads.Bead{
		pendingPoolSessionBead("sess-1"),
		pendingPoolSessionBead("sess-2"),
	}
	verdicts := map[string]AdmissionVerdict{
		"claude": {
			State:       AdmissionDeny,
			Allowed:     false,
			Reason:      "rate limited",
			EvaluatedAt: time.Now(),
		},
	}
	trace := newPoolDesiredStateTestTrace("claude")
	result := computePoolDesiredStates(
		cfg,
		nil,
		nil,
		sessionInfosFromBeads(sessions),
		map[string]int{"claude": 5},
		nil,
		0,
		poolNewDemandLoadVeto{},
		verdicts,
		trace,
	)
	counts := PoolDesiredCounts(result)

	if got := counts["claude"]; got != 2 {
		t.Fatalf("claude desired = %d, want 2 (in-flight sessions must be retained despite admission deny)", got)
	}

	// Verify all returned requests are the in-flight requests, not new anonymous requests.
	for _, req := range result[0].Requests {
		if req.SessionBeadID != "sess-1" && req.SessionBeadID != "sess-2" {
			t.Fatalf("unexpected request %#v, want in-flight session bead retained", req)
		}
	}

	rec := poolTraceDecision(t, trace, TraceSiteAdmissionCheckExec)
	if got := poolTraceFieldInt(t, rec.Fields, "in_flight_retained"); got != 2 {
		t.Fatalf("trace in_flight_retained = %d, want 2", got)
	}
}

// TestAdmissionGate_DenyBypassedByResumeAndFloorGuarantee verifies criterion 5:
// Resume-tier and FloorGuarantee min-fills bypass the gate (deny suppresses growth only).
func TestAdmissionGate_DenyBypassedByResumeAndFloorGuarantee(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("worker", "", intPtr(10), 1)}, // MinActiveSessions = 1
	}
	work := []beads.Bead{
		workBead("w1", "worker", "sess-live", "in_progress", 5),
	}
	sessions := []beads.Bead{sessionBead("sess-live", "open")}
	verdicts := map[string]AdmissionVerdict{
		"worker": {
			State:       AdmissionDeny,
			Allowed:     false,
			Reason:      "deny new demand",
			EvaluatedAt: time.Now(),
		},
	}

	result := computePoolDesiredStates(
		cfg,
		work,
		nil,
		sessionInfosFromBeads(sessions),
		map[string]int{"worker": 5},
		nil,
		0,
		poolNewDemandLoadVeto{},
		verdicts,
		nil,
	)
	counts := PoolDesiredCounts(result)

	// sess-live is resume tier (1). It satisfies MinActiveSessions (1), so no extra min-fill needed.
	if got := counts["worker"]; got != 1 {
		t.Fatalf("worker desired = %d, want 1 (resume request must bypass admission gate)", got)
	}
	if result[0].Requests[0].Tier != "resume" {
		t.Fatalf("request tier = %q, want 'resume'", result[0].Requests[0].Tier)
	}

	// Now test when there is NO live session, but MinActiveSessions = 1.
	// FloorGuarantee should produce 1 session even under admission deny.
	resultMinFill := computePoolDesiredStates(
		cfg,
		nil,
		nil,
		nil,
		map[string]int{"worker": 5},
		nil,
		0,
		poolNewDemandLoadVeto{},
		verdicts,
		nil,
	)
	countsMinFill := PoolDesiredCounts(resultMinFill)
	if got := countsMinFill["worker"]; got != 1 {
		t.Fatalf("worker min-fill desired = %d, want 1 (FloorGuarantee must bypass admission gate)", got)
	}
	if !resultMinFill[0].Requests[0].FloorGuarantee {
		t.Fatalf("request FloorGuarantee = false, want true")
	}
}

// TestAdmissionGate_DenyBypassedByWakeKnownIdentity verifies criterion 5:
// wake-known-identity tier bypasses the admission gate.
func TestAdmissionGate_DenyBypassedByWakeKnownIdentity(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "rig", intPtr(10), 0)},
	}
	// Work assigned to "rig/claude", session is closed.
	work := []beads.Bead{
		workBead("w-closed", "rig/claude", "rig/claude", "in_progress", 5),
	}
	closed := closedPoolSessionBead("sess-dead", "rig/claude")
	verdicts := map[string]AdmissionVerdict{
		"rig/claude": {
			State:       AdmissionDeny,
			Allowed:     false,
			Reason:      "gate closed",
			EvaluatedAt: time.Now(),
		},
	}

	result := computePoolDesiredStates(
		cfg,
		work,
		nil,
		sessionInfosFromBeads([]beads.Bead{closed}),
		map[string]int{"rig/claude": 5},
		nil,
		0,
		poolNewDemandLoadVeto{},
		verdicts,
		nil,
	)
	counts := PoolDesiredCounts(result)

	if got := counts["rig/claude"]; got != 1 {
		t.Fatalf("worker desired = %d, want 1 (wake-known-identity must bypass admission gate)", got)
	}
	if result[0].Requests[0].Tier != "wake-known-identity" {
		t.Fatalf("request tier = %q, want 'wake-known-identity'", result[0].Requests[0].Tier)
	}
}

// TestAdmissionGate_NilVerdictsByteIdentical verifies criterion 3:
// When no admission verdicts are provided, standard scale_check behavior is unchanged.
func TestAdmissionGate_NilVerdictsByteIdentical(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("worker", "", intPtr(10), 0)},
	}
	countsNil := PoolDesiredCounts(ComputePoolDesiredStates(
		cfg,
		nil,
		nil,
		map[string]int{"worker": 4},
	))
	countsWithNilAdmission := PoolDesiredCounts(ComputePoolDesiredStatesWithAdmission(
		cfg,
		nil,
		nil,
		map[string]int{"worker": 4},
		nil,
	))

	if countsNil["worker"] != 4 || countsWithNilAdmission["worker"] != 4 {
		t.Fatalf("nil admission verdicts diverged: standard=%d, withAdmission=%d, want 4", countsNil["worker"], countsWithNilAdmission["worker"])
	}
}

// TestAdmissionGate_RealShellExecution verifies resolveAdmissionVerdicts with real subprocess execution.
func TestAdmissionGate_RealShellExecution(t *testing.T) {
	ClearAdmissionCache()
	cfg := &config.City{
		Admission: config.AdmissionConfig{
			Check:   "echo 'host-all-clear'; exit 0",
			Timeout: "5s",
		},
		Agents: []config.Agent{
			poolAgent("worker1", "", intPtr(10), 0),
			poolAgent("worker2", "", intPtr(10), 0),
		},
	}

	demand := map[string]int{"worker1": 3, "worker2": 2}
	trace := newPoolDesiredStateTestTrace("worker1", "worker2")
	verdicts := resolveAdmissionVerdicts(cfg, t.TempDir(), demand, time.Now(), shellAdmissionCheck, trace)

	if len(verdicts) != 2 {
		t.Fatalf("len(verdicts) = %d, want 2", len(verdicts))
	}
	for _, template := range []string{"worker1", "worker2"} {
		v, ok := verdicts[template]
		if !ok {
			t.Fatalf("missing verdict for %q", template)
		}
		if !v.Allowed || v.State != AdmissionAllow {
			t.Fatalf("verdict for %q = %#v, want allow", template, v)
		}
		if v.Reason != "host-all-clear" {
			t.Fatalf("verdict reason for %q = %q, want 'host-all-clear'", template, v.Reason)
		}
	}

	// Now deny
	ClearAdmissionCache()
	cfgDeny := &config.City{
		Admission: config.AdmissionConfig{
			Check:   "echo 'host-busy'; exit 1",
			Timeout: "5s",
		},
		Agents: []config.Agent{
			poolAgent("worker1", "", intPtr(10), 0),
		},
	}
	verdictsDeny := resolveAdmissionVerdicts(cfgDeny, t.TempDir(), map[string]int{"worker1": 3}, time.Now(), shellAdmissionCheck, nil)
	vDeny := verdictsDeny["worker1"]
	if vDeny.Allowed || vDeny.State != AdmissionDeny {
		t.Fatalf("verdict = %#v, want deny", vDeny)
	}
	if vDeny.Reason != "host-busy" {
		t.Fatalf("verdict reason = %q, want 'host-busy'", vDeny.Reason)
	}
}
