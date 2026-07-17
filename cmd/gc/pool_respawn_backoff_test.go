package main

import (
	"testing"
	"time"
)

// The respawn-backoff tracker is the clock-aware, cross-tick state that throttles
// how fast the pool recreates a generic session for a template after a spawned
// session drained without ever claiming its trigger work (upstream #3279 respawn
// storm). These tests pin the pure timing/edge-detection logic in isolation,
// independent of the buildDesiredState harness.

func testRespawnBackoffConfig() poolRespawnBackoffConfig {
	// base 5s, cap 60s -> windows: 5s, 10s, 20s, 40s, 60s(cap), 60s...
	return poolRespawnBackoffConfig{base: 5 * time.Second, max: 60 * time.Second}
}

// armed reports whether a template is currently backed off (window unelapsed).
// The tracker exposes windowRemaining (magnitude) and activeTemplates (the
// production query); this is a readable boolean shorthand for the tests.
func armed(tr *poolRespawnBackoffTracker, template string, now time.Time) bool {
	return tr.windowRemaining(template, now) > 0
}

func TestPoolRespawnBackoff_DisabledWhenBaseZero(t *testing.T) {
	tr := newPoolRespawnBackoffTracker(poolRespawnBackoffConfig{base: 0, max: time.Minute})
	now := time.Unix(1_000_000, 0)

	tr.observeNoClaimDrain("crm/reviewer", "s-1", now)
	tr.observeNoClaimDrain("crm/reviewer", "s-2", now)

	if armed(tr,"crm/reviewer", now) {
		t.Fatalf("base==0 must fully disable backoff, but template reported backed off")
	}
	if got := tr.activeTemplates(now); len(got) != 0 {
		t.Fatalf("base==0 must yield no active templates, got %v", got)
	}
}

func TestPoolRespawnBackoff_FirstDrainArmsBaseWindow(t *testing.T) {
	tr := newPoolRespawnBackoffTracker(testRespawnBackoffConfig())
	now := time.Unix(1_000_000, 0)

	tr.observeNoClaimDrain("crm/reviewer", "s-1", now)

	// Backed off immediately after the first no-claim drain.
	if !armed(tr,"crm/reviewer", now) {
		t.Fatalf("first no-claim drain must arm the backoff window")
	}
	// A different template is unaffected — de-correlation, not global.
	if armed(tr,"crm/other", now) {
		t.Fatalf("untouched template must not be backed off")
	}
	// The window is at least the base and released once it elapses.
	if !armed(tr,"crm/reviewer", now.Add(4*time.Second)) {
		t.Fatalf("window must still be active within the base duration")
	}
	// Well past the max cap the window has certainly elapsed.
	if armed(tr,"crm/reviewer", now.Add(10*time.Minute)) {
		t.Fatalf("window must release after it elapses")
	}
}

func TestPoolRespawnBackoff_SameDrainCountedOnce(t *testing.T) {
	tr := newPoolRespawnBackoffTracker(testRespawnBackoffConfig())
	now := time.Unix(1_000_000, 0)

	// The SAME drained session bead observed on many ticks must not inflate the
	// consecutive count — otherwise a single lingering drained bead would ramp
	// the window without any real respawn happening.
	tr.observeNoClaimDrain("crm/reviewer", "s-1", now)
	w1 := tr.windowRemaining("crm/reviewer", now)
	tr.observeNoClaimDrain("crm/reviewer", "s-1", now.Add(time.Second))
	tr.observeNoClaimDrain("crm/reviewer", "s-1", now.Add(2*time.Second))
	w2 := tr.windowRemaining("crm/reviewer", now.Add(2*time.Second))

	// Re-observing the same bead id must not have bumped the exponent: remaining
	// window at t+2s should be ~ w1 - 2s, not a fresh larger window.
	if w2 >= w1 {
		t.Fatalf("re-observing the same drained bead inflated the window: w1=%s w2=%s", w1, w2)
	}
}

func TestPoolRespawnBackoff_ExponentialGrowthAcrossDistinctDrains(t *testing.T) {
	tr := newPoolRespawnBackoffTracker(testRespawnBackoffConfig())
	base := time.Unix(1_000_000, 0)

	// Each DISTINCT drained bead id (a real respawn iteration) grows the window.
	// Sample the freshly-armed window right after each drain and assert it grows
	// until it saturates at the cap. Drains are spaced 70s apart: past the prior
	// window (<=60s cap) so it has elapsed, but under resetQuiet (2*max=120s) so
	// the exponent keeps climbing rather than decaying. Jitter is bounded.
	var windows []time.Duration
	for i, id := range []string{"s-1", "s-2", "s-3", "s-4", "s-5", "s-6"} {
		at := base.Add(time.Duration(i) * 70 * time.Second)
		tr.observeNoClaimDrain("crm/reviewer", id, at)
		windows = append(windows, tr.windowRemaining("crm/reviewer", at))
	}

	// Strictly increasing (within jitter slack) until the cap, then bounded.
	// Effective window is [w, 2w) with full-window jitter, so the cap-level
	// window never reaches 2*cap.
	cap := testRespawnBackoffConfig().max
	for i, w := range windows {
		if w >= 2*cap {
			t.Fatalf("window %d = %s reaches 2*cap %s (jitter must stay under a full window)", i, w, cap)
		}
	}
	// The later windows must be materially larger than the first (exponential).
	if windows[len(windows)-1] <= windows[0] {
		t.Fatalf("windows did not grow across distinct drains: first=%s last=%s", windows[0], windows[len(windows)-1])
	}
}

func TestPoolRespawnBackoff_DecayResetsAfterQuiet(t *testing.T) {
	tr := newPoolRespawnBackoffTracker(testRespawnBackoffConfig())
	now := time.Unix(1_000_000, 0)

	// Ramp the exponent with two nearby drains so the window is well above base.
	tr.observeNoClaimDrain("crm/reviewer", "s-1", now)
	tr.observeNoClaimDrain("crm/reviewer", "s-2", now.Add(70*time.Second))
	// Base-level windows are in [base, 2*base); ramped (consecutive=2) is in
	// [2*base, 4*base), so 2*base cleanly separates base-level from ramped.
	base := testRespawnBackoffConfig().base
	ramped := tr.windowRemaining("crm/reviewer", now.Add(70*time.Second))
	if ramped < 2*base {
		t.Fatalf("precondition: window did not ramp above base, got %s", ramped)
	}

	// After a long quiet gap (well beyond resetQuiet=2*max) the storm is over;
	// the next isolated drain must restart from the base window, not the cap.
	quietGap := 10 * time.Minute
	tr.observeNoClaimDrain("crm/reviewer", "s-3", now.Add(70*time.Second).Add(quietGap))
	decayed := tr.windowRemaining("crm/reviewer", now.Add(70*time.Second).Add(quietGap))
	if decayed >= 2*base {
		t.Fatalf("after a quiet gap the window must decay to base, got %s", decayed)
	}
}

func TestPoolRespawnBackoff_ThrottledStormDoesNotDecay(t *testing.T) {
	// A storm that is merely SLOWED by the backoff must keep the exponent
	// saturated at the cap — decay must not mistake a throttled storm for
	// recovery. This models the REALISTIC throttled spacing (current window +
	// worktree-checkout time C), not just the window: each respawn is deferred by
	// the window, then spends C seconds checking out before it drains again. The
	// no-decay guarantee holds in the documented regime where the cap exceeds
	// checkout time (here C=20s < max=60s), so the effective spacing
	// (max + C = 80s) stays under resetQuiet (2*max = 120s).
	cfg := testRespawnBackoffConfig()
	const checkout = 20 * time.Second // C < max, the documented-safe regime
	if checkout >= cfg.max {
		t.Fatalf("test precondition: checkout must be < max to model the safe regime")
	}
	tr := newPoolRespawnBackoffTracker(cfg)
	at := time.Unix(1_000_000, 0)
	var last time.Duration
	for i := 0; i < 8; i++ {
		id := "s-" + string(rune('a'+i))
		tr.observeNoClaimDrain("crm/reviewer", id, at)
		last = tr.windowRemaining("crm/reviewer", at)
		at = at.Add(cfg.max + checkout) // window(cap) + checkout, still < resetQuiet
	}
	if last < cfg.max {
		t.Fatalf("throttled storm must stay pinned near the cap, got %s (cap %s)", last, cfg.max)
	}
}

func TestPoolRespawnBackoff_ConfigurableResetQuiet(t *testing.T) {
	// An explicit resetQuiet overrides the 2*max default. With a LONG resetQuiet,
	// a gap that would decay under the default must NOT decay; the exponent holds.
	cfg := testRespawnBackoffConfig() // base 5s, max 60s -> default resetQuiet 120s
	cfg.resetQuiet = 30 * time.Minute
	tr := newPoolRespawnBackoffTracker(cfg)
	now := time.Unix(1_000_000, 0)

	tr.observeNoClaimDrain("crm/reviewer", "s-1", now)
	tr.observeNoClaimDrain("crm/reviewer", "s-2", now.Add(70*time.Second))
	ramped := tr.windowRemaining("crm/reviewer", now.Add(70*time.Second))

	// A 10-minute gap would decay under the 2*max=120s default, but is well under
	// the configured 30m resetQuiet, so the exponent must keep climbing.
	tr.observeNoClaimDrain("crm/reviewer", "s-3", now.Add(70*time.Second).Add(10*time.Minute))
	after := tr.windowRemaining("crm/reviewer", now.Add(70*time.Second).Add(10*time.Minute))
	if after < ramped {
		t.Fatalf("configured long resetQuiet must prevent decay: ramped=%s after=%s", ramped, after)
	}
}

func TestPoolRespawnBackoff_JitterIsDeterministic(t *testing.T) {
	// Two trackers fed the identical drain sequence must produce identical
	// windows: the jitter is derived from a hash, not an RNG or wallclock, so
	// the behavior is reproducible (and therefore testable/traceable).
	now := time.Unix(1_000_000, 0)
	mk := func() *poolRespawnBackoffTracker {
		tr := newPoolRespawnBackoffTracker(testRespawnBackoffConfig())
		tr.observeNoClaimDrain("crm/reviewer", "s-1", now)
		tr.observeNoClaimDrain("crm/reviewer", "s-2", now.Add(time.Hour))
		return tr
	}
	a := mk().windowRemaining("crm/reviewer", now.Add(time.Hour))
	b := mk().windowRemaining("crm/reviewer", now.Add(time.Hour))
	if a != b {
		t.Fatalf("jitter is non-deterministic: %s vs %s", a, b)
	}
}

func TestPoolRespawnBackoff_ForgetAbsentBoundsMemory(t *testing.T) {
	tr := newPoolRespawnBackoffTracker(testRespawnBackoffConfig())
	now := time.Unix(1_000_000, 0)

	tr.observeNoClaimDrain("crm/reviewer", "s-1", now)
	tr.observeNoClaimDrain("crm/reviewer", "s-2", now)
	// The reaper removed the old drained beads; only s-2 remains live.
	tr.forgetAbsentDrains(map[string]bool{"s-2": true})

	// s-1 forgotten means observing it again counts as a fresh drain (proves the
	// seen-set was pruned rather than growing unbounded).
	before := tr.windowRemaining("crm/reviewer", now.Add(time.Hour))
	tr.observeNoClaimDrain("crm/reviewer", "s-1", now.Add(time.Hour))
	after := tr.windowRemaining("crm/reviewer", now.Add(time.Hour))
	if after <= before {
		t.Fatalf("re-observing a forgotten drain id must count as fresh: before=%s after=%s", before, after)
	}
}
