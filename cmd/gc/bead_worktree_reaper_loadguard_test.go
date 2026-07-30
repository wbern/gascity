package main

import (
	"bytes"
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/events"
)

// The shell reaper has carried a MAX_LOAD throttle all along; the Go reaper had
// none, so a pass competed with whatever else the host was doing. These tests
// cover the port, and in particular the fail direction, which is the opposite of
// the safety gates': skipping a pass costs nothing because the next tick
// reconsiders everything, so an UNREADABLE load must PROCEED. A load probe that
// failed closed would disable the reaper on any host it cannot measure — the
// exact failure mode this feature exists to avoid.

func stubReapLoad(t *testing.T, load float64, err error) {
	t.Helper()
	prev := reapLoadAverageFn
	reapLoadAverageFn = func() (float64, error) { return load, err }
	t.Cleanup(func() { reapLoadAverageFn = prev })
}

func TestReapLoadGuard_SkipsPassWhenLoadExceedsCeiling(t *testing.T) {
	cityPath, cfg, stores := reapThrottleFixture(t, 1, 0)
	pct := 50
	cfg.Daemon.AutoReapClosedBeadWorktreesMaxLoadPercentValue = &pct
	// Well above 50% of however many CPUs this host has.
	stubReapLoad(t, float64(runtime.NumCPU())*10, nil)

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, cfg, stores, nil, false, events.Discard, &stderr)

	if len(report.Reaped) != 0 || len(report.Protected) != 0 {
		t.Fatalf("pass ran under a breached load ceiling: reaped=%d protected=%d", len(report.Reaped), len(report.Protected))
	}
	if !strings.Contains(stderr.String(), "skipping pass") {
		t.Errorf("stderr = %q, want the skip and its reason logged", stderr.String())
	}
}

func TestReapLoadGuard_RunsWhenLoadIsUnderCeiling(t *testing.T) {
	cityPath, cfg, stores := reapThrottleFixture(t, 1, 0)
	pct := 90
	cfg.Daemon.AutoReapClosedBeadWorktreesMaxLoadPercentValue = &pct
	stubReapLoad(t, 0.01, nil)

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, cfg, stores, nil, false, events.Discard, &stderr)

	if len(report.Reaped) != 1 {
		t.Fatalf("Reaped = %d, want 1 on an idle host\nstderr:\n%s", len(report.Reaped), stderr.String())
	}
}

// TestReapLoadGuard_UnreadableLoadProceeds is the fail-direction test and the
// reason this guard cannot be modelled on the safety gates.
func TestReapLoadGuard_UnreadableLoadProceeds(t *testing.T) {
	cityPath, cfg, stores := reapThrottleFixture(t, 1, 0)
	pct := 1 // a ceiling so low that any reading at all breaches it
	cfg.Daemon.AutoReapClosedBeadWorktreesMaxLoadPercentValue = &pct
	// The load value must be HIGH, not zero: a zero alongside the error would sit
	// under every ceiling, so the test would pass whether or not the error is
	// respected. Pairing a breaching value with the error is what makes this
	// discriminating — if the guard ignores err, it skips and the test fails.
	stubReapLoad(t, float64(runtime.NumCPU())*100, errors.New("no load source"))

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, cfg, stores, nil, false, events.Discard, &stderr)

	if len(report.Reaped) != 1 {
		t.Fatalf("Reaped = %d, want 1: an unreadable load must not disable the reaper\nstderr:\n%s", len(report.Reaped), stderr.String())
	}
}

// TestReapLoadGuard_ZeroPercentDisablesTheGuard keeps the default behaviour
// reachable, and the default is 0 precisely because a busy host would otherwise
// skip pass after pass.
func TestReapLoadGuard_ZeroPercentDisablesTheGuard(t *testing.T) {
	cityPath, cfg, stores := reapThrottleFixture(t, 1, 0)
	pct := 0
	cfg.Daemon.AutoReapClosedBeadWorktreesMaxLoadPercentValue = &pct
	stubReapLoad(t, float64(runtime.NumCPU())*100, nil)

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, cfg, stores, nil, false, events.Discard, &stderr)

	if len(report.Reaped) != 1 {
		t.Fatalf("Reaped = %d, want 1 with the guard disabled", len(report.Reaped))
	}
}
