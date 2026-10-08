package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// Four profiles must share three slots even though their per-agent caps allow four.
func TestCapacityGroupAdmission(t *testing.T) {
	for _, tier := range []string{"new", "resume", "wake-known-identity"} {
		t.Run(tier, func(t *testing.T) {
			cfg := &config.City{Workspace: config.Workspace{CapacityGroups: map[string]int{"shared": 3}}}
			var reqs []SessionRequest
			for _, name := range []string{"a", "b", "c", "d"} {
				cfg.Agents = append(cfg.Agents, config.Agent{Name: name, CapacityGroup: "shared", MaxActiveSessions: intPtr(5)})
				reqs = append(reqs, SessionRequest{Template: name, Tier: tier})
			}
			got := applyNestedCaps(cfg, reqs, nil, nil)
			count := 0
			for _, state := range got {
				count += len(state.Requests)
			}
			if count != 3 {
				t.Fatalf("admitted %d requests; want 3", count)
			}
		})
	}
}

func TestCapacityGroupIndependentAndUngrouped(t *testing.T) {
	cfg := &config.City{Workspace: config.Workspace{CapacityGroups: map[string]int{"x": 1, "y": 1}}, Agents: []config.Agent{{Name: "a", CapacityGroup: "x"}, {Name: "b", CapacityGroup: "y"}, {Name: "c"}}}
	got := applyNestedCaps(cfg, []SessionRequest{{Template: "a", Tier: "new"}, {Template: "a", Tier: "new"}, {Template: "b", Tier: "new"}, {Template: "c", Tier: "new"}}, nil, nil)
	count := 0
	for _, s := range got {
		count += len(s.Requests)
	}
	if count != 3 {
		t.Fatalf("got %d", count)
	}
}

// Existing protected work remains desired even after the group is reduced.
func TestCapacityGroupProtectedOversubscription(t *testing.T) {
	cfg := &config.City{Workspace: config.Workspace{CapacityGroups: map[string]int{"x": 1}}, Agents: []config.Agent{{Name: "a", CapacityGroup: "x"}, {Name: "b", CapacityGroup: "x"}}}
	requests := []SessionRequest{{Template: "a", Tier: "new", SessionBeadID: "existing-a"}, {Template: "b", Tier: "new", SessionBeadID: "existing-b"}, {Template: "a", Tier: "new"}}
	got := applyNestedCaps(cfg, requests, nil, nil)
	count := 0
	for _, state := range got {
		for _, req := range state.Requests {
			count++
			if req.SessionBeadID == "" {
				t.Fatal("new demand admitted into oversubscribed group")
			}
		}
	}
	if count != 2 {
		t.Fatalf("protected count=%d, want 2", count)
	}
}

func TestCapacityGroupFloorsAndHeadroom(t *testing.T) {
	cfg := &config.City{Workspace: config.Workspace{CapacityGroups: map[string]int{"x": 1}}, Agents: []config.Agent{{Name: "a", CapacityGroup: "x", MinActiveSessions: intPtr(1)}, {Name: "b", CapacityGroup: "x", MinActiveSessions: intPtr(1)}}}
	got := applyNestedCaps(cfg, nil, nil, nil)
	count := 0
	for _, state := range got {
		count += len(state.Requests)
	}
	if count != 1 {
		t.Fatalf("floor count=%d", count)
	}
	limits := newNestedCapLimits(cfg)
	usage := newNestedCapUsage()
	floors := newNestedCapFloorReservations(cfg, nil, limits, usage)
	if n := capNewDemandCount(limits, usage, floors, &cfg.Agents[1], 3); n != 0 {
		t.Fatalf("headroom=%d; floor for a should reserve only slot", n)
	}
	usage.accept(SessionRequest{Template: "a", Tier: "new"}, limits)
	if n := capNewDemandCount(limits, usage, nil, &cfg.Agents[1], 3); n != 0 {
		t.Fatalf("saturated headroom=%d", n)
	}
}

func TestCapacityGroupExistingCapsAndZero(t *testing.T) {
	for _, dimension := range []string{"agent", "rig", "workspace", "group"} {
		t.Run(dimension, func(t *testing.T) {
			a := poolAgent("one", "rig", intPtr(4), 0)
			a.CapacityGroup = "shared"
			cfg := &config.City{Workspace: config.Workspace{CapacityGroups: map[string]int{"shared": 4}}, Rigs: []config.Rig{{Name: "rig", Path: a.Dir}}, Agents: []config.Agent{a}}
			switch dimension {
			case "agent":
				cfg.Agents[0].MaxActiveSessions = intPtr(0)
			case "rig":
				cfg.Rigs[0].MaxActiveSessions = intPtr(0)
			case "workspace":
				cfg.Workspace.MaxActiveSessions = intPtr(0)
			case "group":
				cfg.Workspace.CapacityGroups["shared"] = 0
			}
			if got := applyNestedCaps(cfg, []SessionRequest{{Template: a.QualifiedName(), Tier: "new"}}, nil, nil); len(got) != 0 {
				t.Fatalf("%s zero cap bypassed: %#v", dimension, got)
			}
		})
	}
}

func TestCapacityGroupProtectedDemandPrepass(t *testing.T) {
	now := time.Date(2026, 10, 8, 17, 0, 0, 0, time.UTC)
	a := poolAgent("claude", "", intPtr(5), 0)
	a.CapacityGroup = "shared"
	cfg := &config.City{Workspace: config.Workspace{CapacityGroups: map[string]int{"shared": 0}}, Agents: []config.Agent{a}}
	sessions := []beads.Bead{protectedPoolSessionBeadAt("existing", now.Add(-time.Second))}
	got := ComputePoolDesiredStatesAt(cfg, nil, sessionInfosFromBeads(sessions), map[string]int{"claude": 3}, now)
	if len(got) != 1 || len(got[0].Requests) != 1 || got[0].Requests[0].SessionBeadID != "existing" {
		t.Fatalf("existing work lost or new demand admitted: %#v", got)
	}
	if copied := deepCopyAgent(&a, "copy", ""); copied.CapacityGroup != "shared" {
		t.Fatalf("pool copy group=%q", copied.CapacityGroup)
	}
}

func TestCapacityGroupTraceAndAccounting(t *testing.T) {
	cfg := &config.City{Workspace: config.Workspace{CapacityGroups: map[string]int{"shared": 1}}, Agents: []config.Agent{{Name: "a", CapacityGroup: "shared"}, {Name: "b", CapacityGroup: "shared"}}}
	limits := newNestedCapLimits(cfg)
	usage := newNestedCapUsage()
	usage.accept(SessionRequest{Template: "a", Tier: "new", SessionBeadID: "existing"}, limits)
	site, reason, payload, rejected := usage.rejection(SessionRequest{Template: "b", Tier: "resume"}, limits)
	if !rejected || site != TraceSitePoolCapacityGroupCap || reason != TraceReasonCapacityGroupCap || payload["capacity_group"] != "shared" {
		t.Fatalf("group rejection=%s %s %#v %v", site, reason, payload, rejected)
	}
	_, reason, limit, current, blockers := newDemandBlockingScope("b", &cfg.Agents[1], limits, usage, 0)
	if reason != TraceReasonCapacityGroupCap || limit != 1 || current != 1 || len(blockers) != 1 || blockers[0].SessionBeadID != "existing" {
		t.Fatalf("blocking scope=%s %d %d %#v", reason, limit, current, blockers)
	}
	adjusted := usageExcludingTemplateNew(usage, "a", "", 1, limits)
	if adjusted.groupCount["shared"] != 0 || usage.groupCount["shared"] != 1 {
		t.Fatal("group adjustment aliased or retained own demand")
	}
}

// Lowering a group cannot evict active assigned work or admit a wake ahead of it.
func TestCapacityGroupLiveAssignedWork(t *testing.T) {
	for _, limit := range []int{0, 1} {
		t.Run(string(rune('0'+limit)), func(t *testing.T) {
			a := poolAgent("one", "", intPtr(5), 0)
			a.CapacityGroup = "shared"
			cfg := &config.City{Workspace: config.Workspace{CapacityGroups: map[string]int{"shared": limit}}, Agents: []config.Agent{a}}
			active := sessionBead("active", "open")
			active.Metadata = map[string]string{"template": "one", "state": "active", "session_name": "active"}
			asleep := sessionBead("asleep", "open")
			asleep.Metadata = map[string]string{"template": "one", "state": "asleep", "session_name": "asleep"}
			work := []beads.Bead{workBead("waiting", "one", "asleep", "in_progress", 0), workBead("working", "one", "active", "in_progress", 5)}
			got := ComputePoolDesiredStates(cfg, work, sessionInfosFromBeads([]beads.Bead{active, asleep}), nil)
			if len(got) != 1 || len(got[0].Requests) != 1 || got[0].Requests[0].SessionBeadID != "active" {
				t.Fatalf("active WIP displaced or extra wake admitted: %#v", got)
			}
		})
	}
}

func TestCapacityGroupDoesNotChangeWorkspacePriority(t *testing.T) {
	cfg := &config.City{Workspace: config.Workspace{MaxActiveSessions: intPtr(1), CapacityGroups: map[string]int{"shared": 10}}, Agents: []config.Agent{{Name: "grouped", CapacityGroup: "shared"}, {Name: "ungrouped"}}}
	got := applyNestedCaps(cfg, []SessionRequest{{Template: "grouped", Tier: "resume", SessionBeadID: "low", ExistingActive: true, BeadPriority: 1}, {Template: "ungrouped", Tier: "resume", SessionBeadID: "high", ExistingActive: true, BeadPriority: 10}}, nil, nil)
	if len(got) != 1 || got[0].Template != "ungrouped" {
		t.Fatalf("nonbinding group changed workspace priority: %#v", got)
	}
}

func TestCapacityGroupProtectedAssignedReuse(t *testing.T) {
	now := time.Date(2026, 10, 8, 17, 0, 0, 0, time.UTC)
	a := poolAgent("claude", "", intPtr(5), 0)
	a.CapacityGroup = "shared"
	cfg := &config.City{Workspace: config.Workspace{CapacityGroups: map[string]int{"shared": 0}}, Agents: []config.Agent{a}}
	protected := protectedPoolSessionBeadAt("existing", now.Add(-time.Second))
	work := []beads.Bead{workBead("assigned", "claude", "claude", "in_progress", 0)}
	got := ComputePoolDesiredStatesAt(cfg, work, sessionInfosFromBeads([]beads.Bead{protected}), nil, now)
	if len(got) != 1 || len(got[0].Requests) != 1 || got[0].Requests[0].SessionBeadID != "existing" || got[0].Requests[0].WorkBeadID != "assigned" {
		t.Fatalf("protected assigned reuse lost at zero: %#v", got)
	}
}
