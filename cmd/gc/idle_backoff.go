package main

import "time"

// idleBackoff implements Pillar 1 (demand-gated ticking) of
// engdocs/design/idle-controller-call-rate.md. The controller's patrol
// interval backs off exponentially from base to ceiling across consecutive
// no-demand passes, and resets to base the moment any demand or wake signal
// is observed. The ceiling doubles as a heartbeat floor: a fully idle
// controller still ticks at the ceiling cadence, guaranteeing forward
// progress even if a wake signal is missed.
//
// idleBackoff is not safe for concurrent use; the controller loop owns it
// and mutates it from a single goroutine.
type idleBackoff struct {
	base    time.Duration
	ceiling time.Duration
	cur     time.Duration
}

// newIdleBackoff returns a scheduler seeded at base. A non-positive base
// falls back to 30s, and a ceiling shorter than base is clamped up to base
// so the interval never drops below the configured patrol cadence.
func newIdleBackoff(base, ceiling time.Duration) *idleBackoff {
	if base <= 0 {
		base = 30 * time.Second
	}
	if ceiling < base {
		ceiling = base
	}
	return &idleBackoff{base: base, ceiling: ceiling, cur: base}
}

// next returns the delay to wait before the next patrol tick and advances
// the scheduler. When demandObserved is true — the pass did work, or a wake
// signal fired — the interval resets to base; otherwise it doubles toward
// the ceiling.
func (b *idleBackoff) next(demandObserved bool) time.Duration {
	if demandObserved {
		b.cur = b.base
		return b.cur
	}
	next := b.cur * 2
	if next > b.ceiling {
		next = b.ceiling
	}
	if next < b.base {
		next = b.base
	}
	b.cur = next
	return b.cur
}

// current returns the interval most recently produced by next (base before
// the first call).
func (b *idleBackoff) current() time.Duration { return b.cur }
