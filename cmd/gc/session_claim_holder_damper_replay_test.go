package main

import (
	"testing"
	"time"
)

// TestDecideClaimHolderRecycleReplaysTheObservedIncident replays the densest
// real run of the gcw-8dbg incident: 15 consecutive
// "claim-holder-stalled ... requesting fresh restart" lines the supervisor
// emitted for gas-city-wbern--architect between 2026-07-24T22:57Z and
// 2026-07-25T06:19Z, roughly one every 31 minutes for seven and a half hours.
// The times are the supervisor log's own, converted from CEST to UTC.
//
// The replay is a COUNTERFACTUAL, not a straight tape-playback, and the
// difference is the whole point. In the log, activity advances after every
// firing because every firing really did restart the session — the restart
// burst landed 74-98 seconds later (median 79s), then the session went silent
// again. Under the damper a SUPPRESSED tick performs no restart, so it
// produces no burst and activity does not advance. Feeding the damper the
// recorded activity instead would credit it with restarts it never performed
// and would reset the counter on its own suppressions.
//
// Within this run there is no genuine recovery to model — the firings are
// continuous, and bd history shows ZERO events on any of the four beads the
// session held across it, so the held-claim fingerprint is constant too.
func TestDecideClaimHolderRecycleReplaysTheObservedIncident(t *testing.T) {
	// The live city's [session] claim_holder_stall_timeout at the time.
	const threshold = 30 * time.Minute
	// The observed restart burst: how long after a recycle the restarted
	// session last showed activity before going quiet again.
	const restartBurst = 79 * time.Second

	observed := []string{
		"2026-07-24T22:57:57Z",
		"2026-07-24T23:29:27Z",
		"2026-07-25T00:00:55Z",
		"2026-07-25T00:32:24Z",
		"2026-07-25T01:03:55Z",
		"2026-07-25T01:36:10Z",
		"2026-07-25T02:07:40Z",
		"2026-07-25T02:39:10Z",
		"2026-07-25T03:10:39Z",
		"2026-07-25T03:42:10Z",
		"2026-07-25T04:13:25Z",
		"2026-07-25T04:45:12Z",
		"2026-07-25T05:16:40Z",
		"2026-07-25T05:48:10Z",
		"2026-07-25T06:19:40Z",
	}

	parse := func(t *testing.T, raw string) time.Time {
		t.Helper()
		at, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			t.Fatalf("parse(%q): %v", raw, err)
		}
		return at
	}

	// Activity before the first recycle in the run, verbatim from that firing's
	// own diagnostic.
	lastActivity := parse(t, "2026-07-24T22:27:58Z")

	state := claimHolderRecycleState{}
	fired, suppressed := 0, 0
	for _, raw := range observed {
		now := parse(t, raw)
		decision := decideClaimHolderRecycle(state, threshold, "1:held", true, lastActivity, now)
		state = decision.Next
		if decision.Fire {
			fired++
			// The recycle happened, so the restart burst happens too.
			lastActivity = now.Add(restartBurst)
			continue
		}
		suppressed++
		// No restart, so no burst: activity stands where the last real recycle
		// left it.
		if !decision.RetryAfter.After(now) {
			t.Fatalf("suppressed at %s but RetryAfter %s had already elapsed", raw, decision.RetryAfter)
		}
	}

	if fired+suppressed != len(observed) {
		t.Fatalf("accounted for %d of %d observed recycles", fired+suppressed, len(observed))
	}
	// Every one of these 15 really fired, and each killed a session holding
	// in-progress work. A backoff that doubles from 30m covers this seven-hour
	// window in a handful of attempts.
	if fired > 5 {
		t.Fatalf("damper allowed %d of %d observed recycles; want no more than 5", fired, len(observed))
	}
	if fired < 1 {
		t.Fatalf("damper suppressed all %d recycles; it must stay a backoff, not a hard cap", len(observed))
	}
	t.Logf("observed %d recycles over %s; damped to %d (%d suppressed)",
		len(observed), parse(t, observed[len(observed)-1]).Sub(parse(t, observed[0])), fired, suppressed)
}
