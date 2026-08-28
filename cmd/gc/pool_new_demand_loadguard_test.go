package main

import (
	"errors"
	"runtime"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// The worktree reaper carries a load guard (bead_worktree_reaper_loadguard_test.go);
// pool new demand had none, so a load spike could not stop the pool from
// spawning more sessions into a saturated host. These tests cover the veto,
// mirroring the reaper's fail direction: skipping new demand costs nothing
// (the next tick reconsiders it), so an UNREADABLE load must PROCEED rather
// than freeze spawns.

func stubPoolNewDemandLoad(t *testing.T, load float64, err error) {
	t.Helper()
	prev := poolNewDemandLoadAverageFn
	poolNewDemandLoadAverageFn = func() (float64, error) { return load, err }
	t.Cleanup(func() { poolNewDemandLoadAverageFn = prev })
}

func TestPoolNewDemandLoadGuard_VetoesDemandWhenLoadExceedsCeiling(t *testing.T) {
	pct := 50
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "rig", intPtr(5), 0)},
	}
	cfg.Daemon.PoolNewDemandMaxLoadPercentValue = &pct
	stubPoolNewDemandLoad(t, float64(runtime.NumCPU())*10, nil)

	result := ComputePoolDesiredStates(cfg, nil, nil, map[string]int{"rig/claude": 3})

	if len(result) != 0 {
		t.Fatalf("result = %#v, want none: new demand should be vetoed under a breached load ceiling", result)
	}
}

// TestPoolNewDemandLoadGuard_RecordsTraceDecision proves the veto is
// diagnosable: a breached ceiling must leave a decision record behind, the
// same way the worktree reaper's identical load guard prints its skip to
// stderr. Silently zeroing demand is indistinguishable from "no work" to an
// operator watching the pool; the trace record is what tells them apart.
func TestPoolNewDemandLoadGuard_RecordsTraceDecision(t *testing.T) {
	pct := 50
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "rig", intPtr(5), 0)},
	}
	cfg.Daemon.PoolNewDemandMaxLoadPercentValue = &pct
	load := float64(runtime.NumCPU()) * 10
	stubPoolNewDemandLoad(t, load, nil)
	trace := newPoolDesiredStateTestTrace("rig/claude")

	result := computePoolDesiredStates(cfg, nil, nil, nil, map[string]int{"rig/claude": 3}, nil, 0, trace)

	if len(result) != 0 {
		t.Fatalf("result = %#v, want none: new demand should be vetoed under a breached load ceiling", result)
	}
	if got := trace.decisionCounts[string(TraceSitePoolNewDemandLoadVeto)]; got != 1 {
		t.Fatalf("load-veto trace decisions = %d, want 1; records=%#v", got, trace.records)
	}
	rec := poolTraceDecision(t, trace, TraceSitePoolNewDemandLoadVeto)
	if rec.ReasonCode != TraceReasonHostLoadVeto {
		t.Fatalf("reason = %q, want %q", rec.ReasonCode, TraceReasonHostLoadVeto)
	}
	if rec.OutcomeCode != TraceOutcomeSkipped {
		t.Fatalf("outcome = %q, want %q", rec.OutcomeCode, TraceOutcomeSkipped)
	}
	if got := poolTraceFieldInt(t, rec.Fields, "max_load_percent"); got != pct {
		t.Fatalf("max_load_percent = %d, want %d", got, pct)
	}
	if got := poolTraceFieldInt(t, rec.Fields, "templates_at_risk"); got != 1 {
		t.Fatalf("templates_at_risk = %d, want 1", got)
	}
}

func TestPoolNewDemandLoadGuard_AllowsDemandWhenLoadIsUnderCeiling(t *testing.T) {
	pct := 90
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "rig", intPtr(5), 0)},
	}
	cfg.Daemon.PoolNewDemandMaxLoadPercentValue = &pct
	stubPoolNewDemandLoad(t, 0.01, nil)

	result := ComputePoolDesiredStates(cfg, nil, nil, map[string]int{"rig/claude": 3})

	if len(result) != 1 || len(result[0].Requests) != 3 {
		t.Fatalf("requests = %#v, want 3 on an idle host", result)
	}
}

// TestPoolNewDemandLoadGuard_UnreadableLoadProceeds is the fail-direction
// test: an unreadable load must not freeze new-session creation.
func TestPoolNewDemandLoadGuard_UnreadableLoadProceeds(t *testing.T) {
	pct := 1 // a ceiling so low that any reading at all breaches it
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "rig", intPtr(5), 0)},
	}
	cfg.Daemon.PoolNewDemandMaxLoadPercentValue = &pct
	// The load value must be HIGH, not zero: a zero alongside the error would
	// sit under every ceiling regardless of whether err is respected.
	stubPoolNewDemandLoad(t, float64(runtime.NumCPU())*100, errors.New("no load source"))

	result := ComputePoolDesiredStates(cfg, nil, nil, map[string]int{"rig/claude": 3})

	if len(result) != 1 || len(result[0].Requests) != 3 {
		t.Fatalf("requests = %#v, want 3: an unreadable load must not veto new demand", result)
	}
}

// TestPoolNewDemandLoadGuard_ZeroPercentDisablesTheGuard keeps the default
// behavior reachable; the default is 0 precisely because a host whose
// baseline already exceeds a naive ceiling would otherwise veto new demand
// on every tick.
func TestPoolNewDemandLoadGuard_ZeroPercentDisablesTheGuard(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "rig", intPtr(5), 0)},
	}
	// PoolNewDemandMaxLoadPercentValue left nil: default is 0, disabled.
	stubPoolNewDemandLoad(t, float64(runtime.NumCPU())*100, nil)

	result := ComputePoolDesiredStates(cfg, nil, nil, map[string]int{"rig/claude": 3})

	if len(result) != 1 || len(result[0].Requests) != 3 {
		t.Fatalf("requests = %#v, want 3 with the guard disabled", result)
	}
}

// TestPoolNewDemandLoadGuard_ResumeTierSurvivesVeto proves the veto's scope:
// it only ever touches new-tier demand. A resume-tier request backing a live
// session with in-progress assigned work must still come out the other
// side — the veto is not a global "stop admitting sessions" switch.
func TestPoolNewDemandLoadGuard_ResumeTierSurvivesVeto(t *testing.T) {
	pct := 50
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "rig", intPtr(5), 0)},
	}
	cfg.Daemon.PoolNewDemandMaxLoadPercentValue = &pct
	stubPoolNewDemandLoad(t, float64(runtime.NumCPU())*10, nil)

	work := []beads.Bead{workBead("w1", "rig/claude", "sess-live", "in_progress", 5)}
	sessions := []beads.Bead{sessionBead("sess-live", "open")}

	result := ComputePoolDesiredStates(cfg, work, sessionInfosFromBeads(sessions), map[string]int{"rig/claude": 3})

	if len(result) != 1 || len(result[0].Requests) != 1 {
		t.Fatalf("requests = %#v, want the resume-tier request to survive a new-demand veto", result)
	}
	req := result[0].Requests[0]
	if req.Tier != "resume" || req.SessionBeadID != "sess-live" {
		t.Fatalf("request = %#v, want the live session's resume request untouched by the veto", req)
	}
}

// TestPoolNewDemandLoadGuard_InFlightSurvivesVeto proves the veto declines to
// ADD load rather than withdrawing capacity the pool has already spent. A
// session already created and mid-start (pending_create_claim / creating)
// represents spent capacity, not new load, and must still be admitted while
// the veto suppresses only the anonymous remainder of scale_check demand.
func TestPoolNewDemandLoadGuard_InFlightSurvivesVeto(t *testing.T) {
	pct := 50
	cfg := &config.City{
		Agents: []config.Agent{poolAgent("claude", "", intPtr(10), 0)},
	}
	cfg.Daemon.PoolNewDemandMaxLoadPercentValue = &pct
	stubPoolNewDemandLoad(t, float64(runtime.NumCPU())*10, nil)

	sessions := []beads.Bead{
		pendingPoolSessionBead("sess-1"),
		pendingPoolSessionBead("sess-2"),
	}
	// scale_check demand exceeds the in-flight count: the anonymous
	// remainder (5-2=3) must be vetoed while the 2 in-flight requests
	// backing sess-1/sess-2 survive.
	scaleCheck := map[string]int{"claude": 5}

	result := ComputePoolDesiredStates(cfg, nil, sessionInfosFromBeads(sessions), scaleCheck)

	if len(result) != 1 || len(result[0].Requests) != 2 {
		t.Fatalf("requests = %#v, want exactly the 2 in-flight requests, none anonymous", result)
	}
	seen := make(map[string]bool)
	for _, req := range result[0].Requests {
		if req.Tier != "new" {
			t.Fatalf("tier = %q, want new (in-flight requests are tier=new): %+v", req.Tier, req)
		}
		if req.SessionBeadID == "" {
			t.Fatalf("veto must not admit an anonymous request: %+v", req)
		}
		seen[req.SessionBeadID] = true
	}
	for _, id := range []string{"sess-1", "sess-2"} {
		if !seen[id] {
			t.Fatalf("missing in-flight request for %s under veto; saw %#v", id, seen)
		}
	}
}
