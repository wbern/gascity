package main

import "time"

// isMinFloorIdleWorker reports whether a session is a legitimate pool floor
// worker that should be exempt from the progress-stall recycler.
//
// A session is a floor worker when the pool has a configured floor
// (minActiveSessions > 0) AND the number of currently open sessions in the
// pool is at or below that floor. In this state every live session is part of
// the always-warm contingent; none should be recycled for being unclaimed —
// they are waiting for routed work, not parked on an error.
//
// Inputs are in-memory values available to the caller; no I/O required.
func isMinFloorIdleWorker(minActiveSessions, openSessionsInPool int) bool {
	return minActiveSessions > 0 && openSessionsInPool <= minActiveSessions
}

// minPositiveDuration returns the smaller of two durations, ignoring
// non-positive values. It returns 0 only when both inputs are non-positive.
// The reconciler uses it to gate the expensive per-session stall checks on the
// tighter of the (independently opt-in) claim-less and claim-holder timeouts, so
// the cheap activity comparison still short-circuits correctly when only one of
// the two recyclers is enabled.
func minPositiveDuration(a, b time.Duration) time.Duration {
	switch {
	case a <= 0:
		return b
	case b <= 0:
		return a
	case a < b:
		return a
	default:
		return b
	}
}

// sessionProgressStalled reports whether a desired, alive session has stopped
// making progress and should be recycled with a fresh restart. It is the
// progress-aware half of the liveness predicate (ADR-0013 Amendment A1, move
// 3b): a live process is necessary but not sufficient for "healthy" — a session
// can be alive yet parked (for example, its turn ended on a provider auth error)
// and will not self-recover, so the reconciler must restart it.
//
// It returns true only when ALL of the following hold:
//   - threshold > 0: the feature is opt-in; an unset/zero timeout disables it.
//   - !holdsClaim: a claimed-but-hung session is the stall-reaper's domain.
//     This targets the claim-less parked case the reaper cannot see (the session
//     parked before it could claim work).
//   - providerHealthy: never recycle a session whose provider cannot currently
//     serve; while a provider is unhealthy the session is left alone until it
//     recovers (composes with the provider-health respawn gate, move 3a).
//   - !exempt: the session is not attached, awaiting interaction, or within its
//     startup grace window.
//   - lastProgress is known and older than threshold.
//
// lastProgress is the most recent provider-reported activity timestamp the
// caller resolved. A zero value means progress is unknown, in which case the
// predicate is conservative and returns false rather than recycle a session
// whose liveness it cannot assess.
func sessionProgressStalled(threshold time.Duration, holdsClaim, providerHealthy, exempt bool, lastProgress, now time.Time) bool {
	if threshold <= 0 || holdsClaim || !providerHealthy || exempt {
		return false
	}
	if lastProgress.IsZero() {
		return false
	}
	return now.Sub(lastProgress) > threshold
}

// sessionClaimHolderStalled reports whether a desired, alive session that HOLDS a
// claim has stopped making progress and should be recycled with a fresh restart.
// It is the mirror of sessionProgressStalled: where that predicate deliberately
// exempts claim-holders, this one targets exactly them — the case no other
// mechanism recovers. A session can hold an in-progress claim yet be wedged when
// its turn ended on a provider condition it will not self-clear (for example a
// codex "Selected model is at capacity" banner) and it will sit indefinitely,
// invisible to every liveness surface (#4012).
//
// It returns true only when ALL of the following hold:
//   - threshold > 0: the feature is opt-in; an unset/zero timeout disables it.
//     Because recycling a claim-holder discards in-progress work, this uses its
//     own, more conservative timeout than the claim-less recycler — set above the
//     longest legitimate quiet period a working claim-holder can exhibit.
//   - holdsClaim: this predicate applies ONLY to claim-holders. The claim-less
//     parked case is sessionProgressStalled's domain.
//   - providerHealthy: never recycle a session whose provider cannot currently
//     serve; leave it alone until the provider recovers.
//   - !exempt: the session is not attached, awaiting interaction, or within its
//     startup grace window.
//   - lastProgress is known and older than threshold.
//
// lastProgress is the most recent provider-reported activity timestamp the caller
// resolved (the poke-discounted value, so gc's own nudges do not mask the stall).
// A zero value means progress is unknown, in which case the predicate is
// conservative and returns false rather than recycle a session it cannot assess.
func sessionClaimHolderStalled(threshold time.Duration, holdsClaim, providerHealthy, exempt bool, lastProgress, now time.Time) bool {
	if threshold <= 0 || !holdsClaim || !providerHealthy || exempt {
		return false
	}
	if lastProgress.IsZero() {
		return false
	}
	return now.Sub(lastProgress) > threshold
}
