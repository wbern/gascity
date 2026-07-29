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
			claims:       "1:aaaa",
			claimsKnown:  true,
			lastActivity: now.Add(-threshold - time.Minute),
			wantFire:     true,
			wantCount:    1,
			wantClaims:   "1:aaaa",
		},
		{
			// The observed incident: recycle at T, restart burst 1m later,
			// quiet again, re-fires one threshold after. Same claims, window
			// far below threshold — the repeat is ineffective, and the second
			// firing is still allowed (backoff applies from count 1 onward).
			name:         "ineffective repeat accrues and re-fires once backoff elapsed",
			prior:        claimHolderRecycleState{Count: 1, At: now.Add(-2 * time.Hour), Claims: "1:aaaa"},
			claims:       "1:aaaa",
			claimsKnown:  true,
			lastActivity: now.Add(-2*time.Hour + time.Minute),
			wantFire:     true,
			wantCount:    2,
			wantClaims:   "1:aaaa",
		},
		{
			name:           "ineffective repeat inside backoff is suppressed",
			prior:          claimHolderRecycleState{Count: 1, At: now.Add(-31 * time.Minute), Claims: "1:aaaa"},
			claims:         "1:aaaa",
			claimsKnown:    true,
			lastActivity:   now.Add(-30 * time.Minute),
			wantFire:       false,
			wantSuppressed: true,
			wantCount:      1,
			wantClaims:     "1:aaaa",
		},
		{
			name:           "backoff grows with the count",
			prior:          claimHolderRecycleState{Count: 3, At: now.Add(-3 * time.Hour), Claims: "1:aaaa"},
			claims:         "1:aaaa",
			claimsKnown:    true,
			lastActivity:   now.Add(-3*time.Hour + time.Minute),
			wantFire:       false,
			wantSuppressed: true,
			wantCount:      3,
			wantClaims:     "1:aaaa",
		},
		{
			// The recycle worked: the worker closed its bead and picked up a
			// different one, so the claim set changed even though the new wedge
			// arrived well inside one threshold.
			name:         "changed claim set resets the counter",
			prior:        claimHolderRecycleState{Count: 4, At: now.Add(-40 * time.Minute), Claims: "1:aaaa"},
			claims:       "1:bbbb",
			claimsKnown:  true,
			lastActivity: now.Add(-31 * time.Minute),
			wantFire:     true,
			wantCount:    1,
			wantClaims:   "1:bbbb",
		},
		{
			// The recycle worked on a long-lived bead the worker never updated:
			// the claim set is unchanged, but the session stayed active for
			// longer than a full threshold, which a startup burst never does.
			name:         "productive activity window resets the counter",
			prior:        claimHolderRecycleState{Count: 4, At: now.Add(-6 * time.Hour), Claims: "1:aaaa"},
			claims:       "1:aaaa",
			claimsKnown:  true,
			lastActivity: now.Add(-6*time.Hour + 45*time.Minute),
			wantFire:     true,
			wantCount:    1,
			wantClaims:   "1:aaaa",
		},
		{
			// Exactly one threshold of post-recycle activity counts as
			// productive (the boundary is inclusive).
			name:         "activity window exactly at threshold is productive",
			prior:        claimHolderRecycleState{Count: 2, At: now.Add(-6 * time.Hour), Claims: "1:aaaa"},
			claims:       "1:aaaa",
			claimsKnown:  true,
			lastActivity: now.Add(-6*time.Hour + threshold),
			wantFire:     true,
			wantCount:    1,
			wantClaims:   "1:aaaa",
		},
		{
			// Activity that predates the recycle is not evidence of anything.
			name:           "activity older than the recycle is not productive",
			prior:          claimHolderRecycleState{Count: 1, At: now.Add(-31 * time.Minute), Claims: "1:aaaa"},
			claims:         "1:aaaa",
			claimsKnown:    true,
			lastActivity:   now.Add(-3 * time.Hour),
			wantFire:       false,
			wantSuppressed: true,
			wantCount:      1,
			wantClaims:     "1:aaaa",
		},
		{
			// An unreadable claim set must not masquerade as progress, and must
			// not overwrite the stored fingerprint with a bogus value either.
			name:           "unknown claim set falls back to the activity window",
			prior:          claimHolderRecycleState{Count: 1, At: now.Add(-31 * time.Minute), Claims: "1:aaaa"},
			claims:         "",
			claimsKnown:    false,
			lastActivity:   now.Add(-30 * time.Minute),
			wantFire:       false,
			wantSuppressed: true,
			wantCount:      1,
			wantClaims:     "1:aaaa",
		},
		{
			name:         "unknown claim set keeps the stored fingerprint when it fires",
			prior:        claimHolderRecycleState{Count: 1, At: now.Add(-2 * time.Hour), Claims: "1:aaaa"},
			claims:       "",
			claimsKnown:  false,
			lastActivity: now.Add(-2*time.Hour + time.Minute),
			wantFire:     true,
			wantCount:    2,
			wantClaims:   "1:aaaa",
		},
		{
			// A stored count with no timestamp cannot be reasoned about; treat
			// it as a first firing rather than suppress on garbage.
			name:         "count without a timestamp is treated as a first firing",
			prior:        claimHolderRecycleState{Count: 7, Claims: "1:aaaa"},
			claims:       "1:aaaa",
			claimsKnown:  true,
			lastActivity: now.Add(-time.Hour),
			wantFire:     true,
			wantCount:    1,
			wantClaims:   "1:aaaa",
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
		decision := decideClaimHolderRecycle(state, threshold, "1:aaaa", true, lastActivity, at)
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
	state := claimHolderRecycleState{Count: 3, At: at, Claims: "2:beefcafe"}

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
		claimHolderRecycleClaimsKey: "1:aaaa",
	})
	got := claimHolderRecycleStateFromInfo(info)
	if got.Count != 0 {
		t.Fatalf("Count = %d, want 0 for an unparsable counter", got.Count)
	}
	if !got.At.IsZero() {
		t.Fatalf("At = %s, want zero for an unparsable stamp", got.At)
	}
	if got.Claims != "1:aaaa" {
		t.Fatalf("Claims = %q, want the raw fingerprint preserved", got.Claims)
	}
}
