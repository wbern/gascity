package main

import (
	"testing"
	"time"
)

// TestWorkQueryTimeoutsAccommodateMultiRoundTripProbe guards the work-query
// timeout budget against the round-trip count the default work-probe actually
// issues (config.Agent.EffectiveWorkQuery, internal/config/workquery.go). The
// earlier budget was sized against an undercount of "~6 round-trips"; the true
// no-work worst case fans out ~15 sequential unpooled bd/store round-trips:
//
//	Tier 1 (in_progress), 3 session identifiers: bd list + bd query(ephemeral) => 6
//	Tier 2 (ready),        3 session identifiers: bd ready + bd query(ephemeral) => 6
//	Tier 3 (pool demand):  bd ready(routed) + bd ready(migration) + bd query   => 3
//
// The assigned tiers all return empty for a freshly-spawned pool worker, so the
// probe pays the full ~15-call scan before the pool-demand tier decides. On a
// remote-dolt city each unpooled call is a fresh Tailscale MySQL connection
// (raw SQL ~0.5s but ~2-5s of connection setup — measured), so under un-park
// concurrent load the scan intermittently exceeded the prior 60s cap, the
// subprocess was killed mid-scan, and the pool worker BLOCKED without claiming
// (and leaked un-reaped). Keeping the budget at ~10s/round-trip across the real
// count clears the realistic loaded cost with margin. The deeper root — routing
// these reads through the warm pooled store so each call is ms, not seconds — is
// tracked on gcw-t9d8 and would let this budget shrink again (a local-dolt city
// already pays ~0s/call and does not need the headroom).
//
// This guards hookWorkQueryTimeout, the cap that actually bounds the work query
// (shellWorkQueryWithEnv in `gc hook` and the workflow serve loop). It does not
// constrain defaultHookRunTimeout: that budget bounds the separate `gc hook run`
// managed-hook wrapper (nudge drain / mail check) and does not enclose the work
// query, so the two are intentionally independent and not asserted against each
// other here.
func TestWorkQueryTimeoutsAccommodateMultiRoundTripProbe(t *testing.T) {
	// minProbeBudget is the remediation target, not merely the old cap: the true
	// probe fans out ~15 unpooled round-trips, so at ~10s/round-trip the budget
	// must clear ~150s. Pinning the floor here means a regression back to the
	// known-bad 60s budget (sized against a ~6-round-trip undercount, which
	// starved and leaked pool workers under load) fails this guard rather than
	// passing it.
	const minProbeBudget = 150 * time.Second

	if hookWorkQueryTimeout < minProbeBudget {
		t.Errorf("hookWorkQueryTimeout = %s, want >= %s (multi-round-trip probe budget)", hookWorkQueryTimeout, minProbeBudget)
	}
}
