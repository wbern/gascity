package main

import (
	"testing"
	"time"

	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// TestClaimHolderRecycleBackoff pins the backoff schedule: no wait before the
// first recycle has been recorded, then a doubling wait bounded by the cap.
func TestClaimHolderRecycleBackoff(t *testing.T) {
	const threshold = 30 * time.Minute
	tests := []struct {
		name      string
		threshold time.Duration
		count     int
		want      time.Duration
	}{
		{name: "no prior recycle", threshold: threshold, count: 0, want: 0},
		{name: "negative count", threshold: threshold, count: -1, want: 0},
		{name: "disabled threshold", threshold: 0, count: 3, want: 0},
		{name: "first repeat doubles", threshold: threshold, count: 1, want: time.Hour},
		{name: "second repeat quadruples", threshold: threshold, count: 2, want: 2 * time.Hour},
		{name: "third repeat", threshold: threshold, count: 3, want: 4 * time.Hour},
		{name: "clamped to cap", threshold: threshold, count: 20, want: claimHolderRecycleBackoffCap},
		{name: "threshold beyond cap is its own backoff", threshold: 48 * time.Hour, count: 5, want: 48 * time.Hour},
		// A short threshold must still reach the cap. Bounding the doubling by
		// a fixed shift instead would top a 5m city out at 5h20m and leave a
		// wedged holder recycling four times a day forever.
		{name: "short threshold still reaches the cap", threshold: 5 * time.Minute, count: 40, want: claimHolderRecycleBackoffCap},
		{name: "one-minute threshold still reaches the cap", threshold: time.Minute, count: 1000, want: claimHolderRecycleBackoffCap},
		// The schedule is unchanged below the cap for a short threshold.
		{name: "short threshold doubles normally", threshold: 5 * time.Minute, count: 3, want: 40 * time.Minute},
		// Just under the cap: one doubling clears it and clamps.
		{name: "threshold just under the cap clamps on the first repeat", threshold: 23 * time.Hour, count: 1, want: claimHolderRecycleBackoffCap},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := claimHolderRecycleBackoff(tc.threshold, tc.count); got != tc.want {
				t.Fatalf("claimHolderRecycleBackoff(%s, %d) = %s, want %s", tc.threshold, tc.count, got, tc.want)
			}
		})
	}
}

// TestDecideClaimHolderRecycle is the core damper truth table. The recycler's
// own restart produces a short startup burst of activity, so "activity
// advanced" can NOT be the reset signal — the observed gcw-8dbg loop advanced
// last-activity on every single one of its 55 firings. The two reset signals
// here are independent: the held CLAIM SET changed (work progressed), or the
// recycle bought at least a full threshold of activity (the session behaved
// differently). Only when NEITHER holds is the repeat treated as ineffective.
func TestDecideClaimHolderRecycle(t *testing.T) {
	const threshold = 30 * time.Minute
	now := time.Date(2026, 7, 25, 3, 36, 0, 0, time.UTC)

	tests := []struct {
		name           string
		prior          claimHolderRecycleState
		claims         string
		claimsKnown    bool
		lastActivity   time.Time
		wantFire       bool
		wantSuppressed bool
		wantCount      int
		wantClaims     string
	}{
		{
			name:         "first firing always recycles",
			prior:        claimHolderRecycleState{},
			claims:       "sc0pe000:1:aaaa",
			claimsKnown:  true,
			lastActivity: now.Add(-threshold - time.Minute),
			wantFire:     true,
			wantCount:    1,
			wantClaims:   "sc0pe000:1:aaaa",
		},
		{
			// The observed incident: recycle at T, restart burst 1m later,
			// quiet again, re-fires one threshold after. Same claims, window
			// far below threshold — the repeat is ineffective, and the second
			// firing is still allowed (backoff applies from count 1 onward).
			name:         "ineffective repeat accrues and re-fires once backoff elapsed",
			prior:        claimHolderRecycleState{Count: 1, At: now.Add(-2 * time.Hour), Claims: "sc0pe000:1:aaaa"},
			claims:       "sc0pe000:1:aaaa",
			claimsKnown:  true,
			lastActivity: now.Add(-2*time.Hour + time.Minute),
			wantFire:     true,
			wantCount:    2,
			wantClaims:   "sc0pe000:1:aaaa",
		},
		{
			name:           "ineffective repeat inside backoff is suppressed",
			prior:          claimHolderRecycleState{Count: 1, At: now.Add(-31 * time.Minute), Claims: "sc0pe000:1:aaaa"},
			claims:         "sc0pe000:1:aaaa",
			claimsKnown:    true,
			lastActivity:   now.Add(-30 * time.Minute),
			wantFire:       false,
			wantSuppressed: true,
			wantCount:      1,
			wantClaims:     "sc0pe000:1:aaaa",
		},
		{
			name:           "backoff grows with the count",
			prior:          claimHolderRecycleState{Count: 3, At: now.Add(-3 * time.Hour), Claims: "sc0pe000:1:aaaa"},
			claims:         "sc0pe000:1:aaaa",
			claimsKnown:    true,
			lastActivity:   now.Add(-3*time.Hour + time.Minute),
			wantFire:       false,
			wantSuppressed: true,
			wantCount:      3,
			wantClaims:     "sc0pe000:1:aaaa",
		},
		{
			// The recycle worked: the worker closed its bead and picked up a
			// different one, so the claim set changed even though the new wedge
			// arrived well inside one threshold.
			name:         "changed claim set resets the counter",
			prior:        claimHolderRecycleState{Count: 4, At: now.Add(-40 * time.Minute), Claims: "sc0pe000:1:aaaa"},
			claims:       "sc0pe000:1:bbbb",
			claimsKnown:  true,
			lastActivity: now.Add(-31 * time.Minute),
			wantFire:     true,
			wantCount:    1,
			wantClaims:   "sc0pe000:1:bbbb",
		},
		{
			// The recycle worked on a long-lived bead the worker never updated:
			// the claim set is unchanged, but the session stayed active for
			// longer than a full threshold, which a startup burst never does.
			name:         "productive activity window resets the counter",
			prior:        claimHolderRecycleState{Count: 4, At: now.Add(-6 * time.Hour), Claims: "sc0pe000:1:aaaa"},
			claims:       "sc0pe000:1:aaaa",
			claimsKnown:  true,
			lastActivity: now.Add(-6*time.Hour + 45*time.Minute),
			wantFire:     true,
			wantCount:    1,
			wantClaims:   "sc0pe000:1:aaaa",
		},
		{
			// Exactly one threshold of post-recycle activity counts as
			// productive (the boundary is inclusive).
			name:         "activity window exactly at threshold is productive",
			prior:        claimHolderRecycleState{Count: 2, At: now.Add(-6 * time.Hour), Claims: "sc0pe000:1:aaaa"},
			claims:       "sc0pe000:1:aaaa",
			claimsKnown:  true,
			lastActivity: now.Add(-6*time.Hour + threshold),
			wantFire:     true,
			wantCount:    1,
			wantClaims:   "sc0pe000:1:aaaa",
		},
		{
			// Activity that predates the recycle is not evidence of anything.
			name:           "activity older than the recycle is not productive",
			prior:          claimHolderRecycleState{Count: 1, At: now.Add(-31 * time.Minute), Claims: "sc0pe000:1:aaaa"},
			claims:         "sc0pe000:1:aaaa",
			claimsKnown:    true,
			lastActivity:   now.Add(-3 * time.Hour),
			wantFire:       false,
			wantSuppressed: true,
			wantCount:      1,
			wantClaims:     "sc0pe000:1:aaaa",
		},
		{
			// An unreadable claim set must not masquerade as progress, and must
			// not overwrite the stored fingerprint with a bogus value either.
			name:           "unknown claim set falls back to the activity window",
			prior:          claimHolderRecycleState{Count: 1, At: now.Add(-31 * time.Minute), Claims: "sc0pe000:1:aaaa"},
			claims:         "",
			claimsKnown:    false,
			lastActivity:   now.Add(-30 * time.Minute),
			wantFire:       false,
			wantSuppressed: true,
			wantCount:      1,
			wantClaims:     "sc0pe000:1:aaaa",
		},
		{
			name:         "unknown claim set keeps the stored fingerprint when it fires",
			prior:        claimHolderRecycleState{Count: 1, At: now.Add(-2 * time.Hour), Claims: "sc0pe000:1:aaaa"},
			claims:       "",
			claimsKnown:  false,
			lastActivity: now.Add(-2*time.Hour + time.Minute),
			wantFire:     true,
			wantCount:    2,
			wantClaims:   "sc0pe000:1:aaaa",
		},
		{
			// A stored count with no timestamp cannot be reasoned about; treat
			// it as a first firing rather than suppress on garbage.
			name:         "count without a timestamp is treated as a first firing",
			prior:        claimHolderRecycleState{Count: 7, Claims: "sc0pe000:1:aaaa"},
			claims:       "sc0pe000:1:aaaa",
			claimsKnown:  true,
			lastActivity: now.Add(-time.Hour),
			wantFire:     true,
			wantCount:    1,
			wantClaims:   "sc0pe000:1:aaaa",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := decideClaimHolderRecycle(tc.prior, threshold, tc.claims, tc.claimsKnown, tc.lastActivity, now)
			if got.Fire != tc.wantFire {
				t.Fatalf("Fire = %v, want %v", got.Fire, tc.wantFire)
			}
			if got.Suppressed != tc.wantSuppressed {
				t.Fatalf("Suppressed = %v, want %v", got.Suppressed, tc.wantSuppressed)
			}
			if got.Next.Count != tc.wantCount {
				t.Fatalf("Next.Count = %d, want %d", got.Next.Count, tc.wantCount)
			}
			if got.Next.Claims != tc.wantClaims {
				t.Fatalf("Next.Claims = %q, want %q", got.Next.Claims, tc.wantClaims)
			}
			if got.Fire && !got.Next.At.Equal(now) {
				t.Fatalf("Next.At = %s, want %s", got.Next.At, now)
			}
			if got.Suppressed && !got.Next.At.Equal(tc.prior.At) {
				t.Fatalf("suppressed Next.At = %s, want the prior stamp %s", got.Next.At, tc.prior.At)
			}
		})
	}
}

// TestClaimFingerprintIsScopedToTheStoresItRead pins the property the damper's
// progress signal rests on: a fingerprint identifies the store set it was read
// from as well as the beads it found.
//
// rigStores is rebuilt wholesale on every config reload and silently skips any
// rig that is unbound or whose store fails to open, so the reconciler's view of
// the stores genuinely varies between ticks. Without the scope, a rig dropping
// out would shrink the claim set and read as "the work moved on".
func TestClaimFingerprintIsScopedToTheStoresItRead(t *testing.T) {
	held := map[string]struct{}{"gc2-aaaa": {}, "gc2-bbbb": {}}
	cityOnly := claimFingerprint([]string{claimScopeCityStore}, held)
	withRig := claimFingerprint([]string{claimScopeCityStore, claimScopeRigStorePrefix + "gascity"}, held)

	if cityOnly == withRig {
		t.Fatalf("identical fingerprint %q for two different store sets; a rig dropping out would be invisible", cityOnly)
	}
	if claimsComparable(cityOnly, withRig) {
		t.Fatal("fingerprints from different store sets compared as comparable; a shrinking claim set would read as progress")
	}
	// Order of the store set must not matter — rigStores is a map.
	shuffled := claimFingerprint([]string{claimScopeRigStorePrefix + "gascity", claimScopeCityStore}, held)
	if shuffled != withRig {
		t.Fatalf("fingerprint depends on store-set order: %q vs %q", shuffled, withRig)
	}
	// Same scope, different beads: that IS progress.
	moved := claimFingerprint([]string{claimScopeCityStore}, map[string]struct{}{"gc2-cccc": {}})
	if !claimsComparable(cityOnly, moved) {
		t.Fatal("same store set compared as incomparable")
	}
	if cityOnly == moved {
		t.Fatal("different claim sets produced the same fingerprint")
	}
	// An emptied claim set is still scoped, so "the worker finished its bead"
	// stays distinguishable from "we could not see the store it was in".
	emptied := claimFingerprint([]string{claimScopeCityStore}, nil)
	if !claimsComparable(cityOnly, emptied) || emptied == cityOnly {
		t.Fatalf("emptied claim set %q must be comparable to, and different from, %q", emptied, cityOnly)
	}
	// A rig literally named "city" must not alias the city store's own entry.
	if claimFingerprint([]string{claimScopeRigStorePrefix + "city"}, held) == claimFingerprint([]string{claimScopeCityStore}, held) {
		t.Fatal("a rig named \"city\" collided with the city store's scope entry")
	}
}

// TestDecideClaimHolderRecycleIgnoresClaimChangeFromADifferentStoreSet is the
// behavioral half of the scope property: when the store set moved, a changed
// claim set is evidence about the reconciler's view, not about the work, so it
// must NOT reset the damper. The decision falls back to the activity window —
// the same conservative path an unreadable claim set takes.
func TestDecideClaimHolderRecycleIgnoresClaimChangeFromADifferentStoreSet(t *testing.T) {
	const threshold = 30 * time.Minute
	now := time.Date(2026, 7, 25, 3, 36, 0, 0, time.UTC)
	prior := claimHolderRecycleState{
		Count:  2,
		At:     now.Add(-31 * time.Minute),
		Claims: claimFingerprint([]string{claimScopeCityStore, claimScopeRigStorePrefix + "gascity"}, map[string]struct{}{"gc2-aaaa": {}}),
	}
	// The rig store dropped out of this tick, so only the city store was read.
	narrowed := claimFingerprint([]string{claimScopeCityStore}, map[string]struct{}{})

	got := decideClaimHolderRecycle(prior, threshold, narrowed, true, prior.At.Add(time.Minute), now)

	if got.Fire {
		t.Fatal("Fire = true; a claim set that shrank because a store was not read is not progress")
	}
	if !got.Suppressed {
		t.Fatal("Suppressed = false, want the repeat still damped")
	}
	if got.Next.Count != prior.Count {
		t.Fatalf("Next.Count = %d, want the accrued %d kept", got.Next.Count, prior.Count)
	}
	// The same reading, once the store set is back, must still be able to
	// signal progress — the scope guard suppresses the comparison, not the
	// signal.
	sameScope := claimFingerprint([]string{claimScopeCityStore, claimScopeRigStorePrefix + "gascity"}, map[string]struct{}{"gc2-cccc": {}})
	if again := decideClaimHolderRecycle(prior, threshold, sameScope, true, prior.At.Add(time.Minute), now); !again.Fire || again.Next.Count != 1 {
		t.Fatalf("same-scope claim change did not reset: Fire=%v Count=%d", again.Fire, again.Next.Count)
	}
}

// TestDecideClaimHolderRecycleRepairsAStampInTheFuture covers the one way the
// damper could fail CLOSED and stay there. Every other unusable marker reads as
// "no state" and falls back to today's behavior, but a stamp in the FUTURE
// parses fine: now.Sub(prior.At) is negative, so it is below any wait, and the
// suppressed path rewrites nothing — the session would never be recycled again
// until wall-clock caught up, which for a corrupt stamp is never.
//
// The repair must not overcorrect either. Firing immediately would turn a
// two-second backward NTP step into an extra destructive restart of a session
// holding in-progress work. So the tick is still suppressed; what changes is
// that the stamp is rewritten to now, which re-bases the backoff on a value the
// damper trusts and guarantees the session is reconsidered one window later.
func TestDecideClaimHolderRecycleRepairsAStampInTheFuture(t *testing.T) {
	const threshold = 30 * time.Minute
	now := time.Date(2026, 7, 25, 3, 36, 0, 0, time.UTC)

	for _, tc := range []struct {
		name string
		at   time.Time
	}{
		{name: "small backward clock step", at: now.Add(2 * time.Second)},
		{name: "hand-edited far-future stamp", at: now.AddDate(73, 0, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prior := claimHolderRecycleState{Count: 2, At: tc.at, Claims: "sc0pe000:1:aaaa"}
			got := decideClaimHolderRecycle(prior, threshold, "sc0pe000:1:aaaa", true, now.Add(-time.Hour), now)

			if got.Fire {
				t.Fatal("Fire = true; a stamp the damper cannot time must not trigger a destructive restart")
			}
			if !got.Suppressed {
				t.Fatal("Suppressed = false, want the tick suppressed while the stamp is repaired")
			}
			if !got.Persist {
				t.Fatal("Persist = false; the repaired stamp must be written or the session stays wedged forever")
			}
			if !got.Next.At.Equal(now) {
				t.Fatalf("Next.At = %s, want it re-based on now (%s)", got.Next.At, now)
			}
			if got.Next.Count != prior.Count {
				t.Fatalf("Next.Count = %d, want the accrued count %d preserved through the repair", got.Next.Count, prior.Count)
			}
			// The whole point of the repair: the next attempt is now reachable.
			if !got.RetryAfter.After(now) || got.RetryAfter.Sub(now) > claimHolderRecycleBackoffCap {
				t.Fatalf("RetryAfter = %s, want a due time within one backoff window of %s", got.RetryAfter, now)
			}
		})
	}
}

// TestDecideClaimHolderRecycleConvergesOnTheObservedLoop replays the shape of
// the gcw-8dbg incident — a session whose restart can never clear its stall,
// re-firing one threshold after each recycle with the same held claim — and
// asserts the damper turns an unbounded stream of recycles into a logarithmic
// one while never stopping retrying altogether.
func TestDecideClaimHolderRecycleConvergesOnTheObservedLoop(t *testing.T) {
	const threshold = 30 * time.Minute
	now := time.Date(2026, 7, 24, 23, 45, 0, 0, time.UTC)
	deadline := now.Add(96 * time.Hour) // the four days the incident ran

	state := claimHolderRecycleState{}
	fired := 0
	// Tick every minute, mirroring the reconciler cadence closely enough.
	for at := now; at.Before(deadline); at = at.Add(time.Minute) {
		// The stall predicate is true whenever the session has been quiet for
		// a threshold. After a recycle the restart burst lands one minute
		// later, so activity is always "last recycle + 1m".
		lastActivity := state.At.Add(time.Minute)
		if state.At.IsZero() {
			lastActivity = now.Add(-2 * threshold)
		}
		if at.Sub(lastActivity) <= threshold {
			continue
		}
		decision := decideClaimHolderRecycle(state, threshold, "sc0pe000:1:aaaa", true, lastActivity, at)
		state = decision.Next
		if decision.Fire {
			fired++
		}
	}

	// Undamped this is 96h/31m ≈ 185 recycles. With the backoff it must be a
	// small number, and it must be non-zero — a wedged worker whose provider
	// capacity returns later still gets retried.
	if fired == 0 {
		t.Fatal("damper stopped retrying entirely; a wedged holder must still get periodic recycles")
	}
	if fired > 12 {
		t.Fatalf("damper allowed %d recycles over 4 days; want a bounded handful", fired)
	}
}

// TestClaimHolderRecycleStateRoundTrip proves the damper state survives the
// recycle it is counting: it is projected off the SESSION bead, which the
// restart handoff patches in place rather than replacing.
func TestClaimHolderRecycleStateRoundTrip(t *testing.T) {
	at := time.Date(2026, 7, 25, 3, 36, 0, 0, time.UTC)
	state := claimHolderRecycleState{Count: 3, At: at, Claims: "sc0pe000:2:beefcafe"}

	info := sessionpkg.Info{}.ApplyPatch(state.patch())
	got := claimHolderRecycleStateFromInfo(info)

	if got.Count != state.Count {
		t.Fatalf("Count = %d, want %d", got.Count, state.Count)
	}
	if !got.At.Equal(state.At) {
		t.Fatalf("At = %s, want %s", got.At, state.At)
	}
	if got.Claims != state.Claims {
		t.Fatalf("Claims = %q, want %q", got.Claims, state.Claims)
	}

	// The restart handoff must not clear the damper state — that would reset
	// the counter on every recycle and restore the unbounded loop.
	restarted := info.ApplyPatch(sessionpkg.RestartRequestPatch("", at))
	if after := claimHolderRecycleStateFromInfo(restarted); after.Count != state.Count || !after.At.Equal(state.At) || after.Claims != state.Claims {
		t.Fatalf("damper state after RestartRequestPatch = %+v, want it preserved as %+v", after, state)
	}
}

// TestClaimHolderRecycleStateFromInfoTolerantOfGarbage keeps a corrupt or
// hand-edited marker from wedging the reconciler: unparsable values read as
// "no damper state", which fails open to today's behavior.
func TestClaimHolderRecycleStateFromInfoTolerantOfGarbage(t *testing.T) {
	info := sessionpkg.Info{}.ApplyPatch(sessionpkg.MetadataPatch{
		claimHolderRecycleCountKey:  "not-a-number",
		claimHolderRecycleAtKey:     "not-a-time",
		claimHolderRecycleClaimsKey: "sc0pe000:1:aaaa",
	})
	got := claimHolderRecycleStateFromInfo(info)
	if got.Count != 0 {
		t.Fatalf("Count = %d, want 0 for an unparsable counter", got.Count)
	}
	if !got.At.IsZero() {
		t.Fatalf("At = %s, want zero for an unparsable stamp", got.At)
	}
	if got.Claims != "sc0pe000:1:aaaa" {
		t.Fatalf("Claims = %q, want the raw fingerprint preserved", got.Claims)
	}

	// A padded fingerprint must read as the same value, not as a perpetually
	// changed one. The damper never writes surrounding space, so padding is a
	// hand edit; comparing it raw would make every freshly computed fingerprint
	// differ from it, reset the counter on every tick, and restore the
	// unbounded loop through nothing worse than a stray space.
	padded := sessionpkg.Info{}.ApplyPatch(sessionpkg.MetadataPatch{
		claimHolderRecycleCountKey:  " 3 ",
		claimHolderRecycleAtKey:     " 2026-07-25T03:36:00Z ",
		claimHolderRecycleClaimsKey: "  sc0pe000:1:aaaa  ",
	})
	trimmed := claimHolderRecycleStateFromInfo(padded)
	if trimmed.Claims != "sc0pe000:1:aaaa" {
		t.Fatalf("Claims = %q, want the padded fingerprint trimmed to match a freshly computed one", trimmed.Claims)
	}
	if trimmed.Count != 3 || trimmed.At.IsZero() {
		t.Fatalf("padded count/stamp did not parse: %+v", trimmed)
	}
}
