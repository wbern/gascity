package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

// The Go reaper had no per-pass cap, no pacing and no load guard: it iterated
// every candidate in every rig on every controller tick. That was survivable
// only while the reaper reaped nothing. Once the liveness scan started working
// on this platform the live dry-run went from 0 to 131 eligible worktrees, so an
// unpaced pass now means 131 `git worktree remove` calls back to back — the
// cold-start thrash gci-jyh5 predicted. The bash reaper has carried
// MAX_PER_RUN/MAX_LOAD/SLEEP throttles all along; these tests port the cap and
// the pacing.

// reapThrottleFixture builds n reapable closed-bead worktrees in one rig and
// returns everything reapClosedBeadWorktrees needs, with the per-pass cap set.
func reapThrottleFixture(t *testing.T, n, maxPerPass int) (string, *config.City, map[string]beads.Store) {
	t.Helper()
	cityPath, rigRoot := initReapRig(t)
	list := make([]beads.Bead, 0, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("ga-thr%03d", i)
		addClosedWorktree(t, rigRoot, cityPath, "builder", id)
		list = append(list, beads.Bead{ID: id, Status: "closed"})
	}
	cfg := reapTestConfig(rigRoot)
	cfg.Daemon.AutoReapClosedBeadWorktreesMaxPerPassCount = &maxPerPass
	injectLiveness(t, liveWorktreeState{scanned: true})
	return cityPath, cfg, map[string]beads.Store{"mrig": beads.NewMemStoreFrom(1, list, nil)}
}

// TestReapClosedBeadWorktrees_HonoursPerPassCap bounds the blast radius of a
// single pass. Without it, a first pass after a long inert period removes every
// accumulated worktree at once.
func TestReapClosedBeadWorktrees_HonoursPerPassCap(t *testing.T) {
	cityPath, cfg, stores := reapThrottleFixture(t, 5, 2)

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, cfg, stores, nil, false, events.Discard, &stderr)

	if len(report.Reaped) != 2 {
		t.Fatalf("Reaped = %d entries, want 2 (the cap)\nstderr:\n%s", len(report.Reaped), stderr.String())
	}
}

// TestReapClosedBeadWorktrees_DeferredByCapIsReported is the no-silent-truncation
// rule: a pass that stopped early must say so, or "3 kept" reads as "3 were
// unsafe" when they were merely deferred.
func TestReapClosedBeadWorktrees_DeferredByCapIsReported(t *testing.T) {
	cityPath, cfg, stores := reapThrottleFixture(t, 5, 2)

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, cfg, stores, nil, false, events.Discard, &stderr)

	deferred := 0
	for _, d := range report.Protected {
		if strings.Contains(d.Reason, "cap") {
			deferred++
		}
	}
	if deferred != 3 {
		t.Fatalf("deferred-by-cap entries = %d, want 3; Protected = %+v", deferred, report.Protected)
	}
	if !strings.Contains(stderr.String(), "cap") {
		t.Errorf("stderr = %q, want the cap named in the log", stderr.String())
	}
}

// TestReapClosedBeadWorktrees_ZeroCapMeansUnlimited keeps the default behavior
// available and makes "no cap configured" explicit rather than accidentally zero.
func TestReapClosedBeadWorktrees_ZeroCapMeansUnlimited(t *testing.T) {
	cityPath, cfg, stores := reapThrottleFixture(t, 4, 0)

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, cfg, stores, nil, false, events.Discard, &stderr)

	if len(report.Reaped) != 4 {
		t.Fatalf("Reaped = %d, want all 4 with the cap disabled\nstderr:\n%s", len(report.Reaped), stderr.String())
	}
}

// TestReapClosedBeadWorktrees_DryRunIgnoresTheCap matters because the dry-run is
// the operator's inventory. Capping it would under-report the backlog and make
// the readout depend on a pacing knob that removes nothing.
func TestReapClosedBeadWorktrees_DryRunIgnoresTheCap(t *testing.T) {
	cityPath, cfg, stores := reapThrottleFixture(t, 5, 2)

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, cfg, stores, nil, true, events.Discard, &stderr)

	if len(report.Reaped) != 5 {
		t.Fatalf("dry-run would-reap = %d, want all 5: a cap paces removals, it must not hide the backlog", len(report.Reaped))
	}
}

// TestReapClosedBeadWorktrees_PacesBetweenRemovals proves the sleep is wired.
// Real time is not consumed: the pacing hook is injected.
func TestReapClosedBeadWorktrees_PacesBetweenRemovals(t *testing.T) {
	cityPath, cfg, stores := reapThrottleFixture(t, 3, 0)

	paced := 0
	prev := reapPaceFn
	reapPaceFn = func(time.Duration) { paced++ }
	t.Cleanup(func() { reapPaceFn = prev })

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, cfg, stores, nil, false, events.Discard, &stderr)

	if len(report.Reaped) != 3 {
		t.Fatalf("Reaped = %d, want 3", len(report.Reaped))
	}
	if paced != 3 {
		t.Errorf("pace hook called %d times, want once per removal (3)", paced)
	}
}

// TestReapClosedBeadWorktrees_DryRunDoesNotPace — nothing is removed, so there
// is no I/O to pace, and a dry-run over hundreds of trees must stay fast enough
// to be worth running.
func TestReapClosedBeadWorktrees_DryRunDoesNotPace(t *testing.T) {
	cityPath, cfg, stores := reapThrottleFixture(t, 3, 0)

	paced := 0
	prev := reapPaceFn
	reapPaceFn = func(time.Duration) { paced++ }
	t.Cleanup(func() { reapPaceFn = prev })

	var stderr bytes.Buffer
	reapClosedBeadWorktrees(cityPath, cfg, stores, nil, true, events.Discard, &stderr)

	if paced != 0 {
		t.Errorf("pace hook called %d times under dry-run, want 0", paced)
	}
}
