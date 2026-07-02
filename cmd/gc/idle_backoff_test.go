package main

import (
	"testing"
	"time"
)

// TestIdleBackoffDoublesToCeilingThenHolds pins the core Pillar-1 schedule:
// consecutive no-demand passes double the interval from base toward the
// ceiling, then hold at the ceiling (the heartbeat floor).
func TestIdleBackoffDoublesToCeilingThenHolds(t *testing.T) {
	t.Parallel()
	b := newIdleBackoff(30*time.Second, 5*time.Minute)
	want := []time.Duration{
		1 * time.Minute, // 30s * 2
		2 * time.Minute, // 1m * 2
		4 * time.Minute, // 2m * 2
		5 * time.Minute, // 4m * 2 = 8m, capped at ceiling
		5 * time.Minute, // holds at ceiling (heartbeat floor)
		5 * time.Minute,
	}
	for i, w := range want {
		if got := b.next(false); got != w {
			t.Fatalf("idle pass %d: next(false) = %v, want %v", i+1, got, w)
		}
	}
}

// TestIdleBackoffResetsToBaseOnDemand verifies any demand/wake resets the
// interval to base, and the backoff then restarts from base.
func TestIdleBackoffResetsToBaseOnDemand(t *testing.T) {
	t.Parallel()
	b := newIdleBackoff(30*time.Second, 5*time.Minute)
	b.next(false) // 1m
	b.next(false) // 2m
	if got := b.next(true); got != 30*time.Second {
		t.Fatalf("next(true) = %v, want reset to base 30s", got)
	}
	if got := b.next(false); got != 1*time.Minute {
		t.Fatalf("post-reset idle pass: next(false) = %v, want 1m", got)
	}
}

// TestIdleBackoffClampsCeilingBelowBase guards a misconfiguration where the
// ceiling is shorter than the base — the interval must never drop below base.
func TestIdleBackoffClampsCeilingBelowBase(t *testing.T) {
	t.Parallel()
	b := newIdleBackoff(1*time.Minute, 10*time.Second)
	if got := b.next(false); got != 1*time.Minute {
		t.Fatalf("next(false) = %v, want base 1m (ceiling clamped up to base)", got)
	}
}

// TestIdleBackoffDefaultsBaseWhenNonPositive guards a zero/negative base.
func TestIdleBackoffDefaultsBaseWhenNonPositive(t *testing.T) {
	t.Parallel()
	b := newIdleBackoff(0, 0)
	if got := b.current(); got != 30*time.Second {
		t.Fatalf("current() = %v, want default base 30s", got)
	}
	if got := b.next(false); got != 30*time.Second {
		t.Fatalf("next(false) = %v, want base 30s (ceiling clamped to base)", got)
	}
}
