package main

import (
	"errors"
	"runtime"
	"testing"

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
