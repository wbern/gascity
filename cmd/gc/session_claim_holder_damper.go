package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// Damper for the claim-holder stall recycler (gcw-8dbg).
//
// The recycler restarts a session that holds an in-progress claim but has gone
// quiet, on the theory that it wedged mid-work on a provider condition it will
// not self-clear. When that theory is right the restart clears the wedge. When
// it is wrong — the stall has a cause a restart cannot touch — the recycler
// re-evaluates the same predicate on the next tick and takes the same
// destructive action again, forever: the observed incident ran 55 recycles of
// one session over four days, each discarding whatever the session held.
//
// Nothing else stops it. Both existing dampers (checkStability's wake_attempts
// and checkChurn's churn_count) key off last_woke_at, and the restart handoff
// clears last_woke_at deliberately — that is what "masks the intentional death
// from crash and churn trackers" means at the RestartRequestPatch call site. An
// autonomous recycle therefore accrues nothing anywhere. This file gives that
// one path its own accounting.
//
// The reset signal is the subtle part. "Last activity advanced" does NOT work:
// the restart itself produces a startup burst, so activity advanced on every
// one of the 55 firings and any freshness-keyed reset re-arms the loop
// immediately. Two signals that survive that objection are used instead, and
// either one alone clears the counter:
//
//   - the held CLAIM SET changed — the work moved, so the recycle did its job
//     even if the session wedged again quickly;
//   - the recycle bought at least one full threshold of activity — the session
//     behaved differently afterward, which a startup burst never does. Reusing
//     the stall threshold rather than a fresh constant keeps this self-scaling:
//     whatever quiet period a city calls a stall is the same period it takes to
//     call the recovery real.
//
// Only when NEITHER holds is a repeat counted as ineffective, and the response
// is a growing suppression window rather than a hard cap: a worker wedged on
// provider capacity that returns tomorrow must still get recycled tomorrow.
const (
	claimHolderRecycleCountKey  = "claim_holder_recycle_count"
	claimHolderRecycleAtKey     = "claim_holder_recycle_at"
	claimHolderRecycleClaimsKey = "claim_holder_recycle_claims"

	// claimHolderRecycleBackoffMaxShift bounds the doubling so the shift below
	// cannot overflow time.Duration for a large configured threshold.
	claimHolderRecycleBackoffMaxShift = 6

	// claimHolderRecycleBackoffCap is the longest a session is left un-recycled
	// on account of the damper. A day is long enough that a repeating
	// ineffective restart stops being a source of churn, and short enough that a
	// genuinely wedged worker is retried within an operator's normal horizon.
	claimHolderRecycleBackoffCap = 24 * time.Hour
)

// claimHolderRecycleState is the damper's persisted accounting for one session:
// how many consecutive claim-holder recycles have changed nothing, when the
// most recent one fired, and the fingerprint of the claims it held then.
type claimHolderRecycleState struct {
	Count  int
	At     time.Time
	Claims string
}

// claimHolderRecycleDecision is the damper's verdict for one stalled
// claim-holder: whether to recycle, the accounting to persist, and — when the
// recycle is being withheld — when the next attempt becomes due.
type claimHolderRecycleDecision struct {
	Fire       bool
	Suppressed bool
	Next       claimHolderRecycleState
	RetryAfter time.Time
}

// claimHolderRecycleBackoff returns how long to wait after a recycle before
// repeating it, given how many consecutive recycles have already changed
// nothing. It doubles per ineffective repeat, clamped to
// claimHolderRecycleBackoffCap. A threshold at or beyond the cap is its own
// backoff — a city that already waits that long for a stall should not have the
// damper shorten it.
func claimHolderRecycleBackoff(threshold time.Duration, count int) time.Duration {
	if threshold <= 0 || count < 1 {
		return 0
	}
	if threshold >= claimHolderRecycleBackoffCap {
		return threshold
	}
	shift := count
	if shift > claimHolderRecycleBackoffMaxShift {
		shift = claimHolderRecycleBackoffMaxShift
	}
	if backoff := threshold << shift; backoff < claimHolderRecycleBackoffCap {
		return backoff
	}
	return claimHolderRecycleBackoffCap
}

// decideClaimHolderRecycle decides whether a stalled claim-holder should
// actually be recycled now. The caller has already established that the stall
// predicate holds; this only answers "and has repeating it been achieving
// anything?".
//
// claims is the fingerprint of the claims the session holds right now and
// claimsKnown reports whether that fingerprint could be read at all. An
// unreadable fingerprint is not allowed to masquerade as progress, nor to
// overwrite the stored one — the activity window carries the decision alone.
func decideClaimHolderRecycle(
	prior claimHolderRecycleState,
	threshold time.Duration,
	claims string,
	claimsKnown bool,
	lastActivity, now time.Time,
) claimHolderRecycleDecision {
	nextClaims := claims
	if !claimsKnown {
		nextClaims = prior.Claims
	}
	fire := func(count int) claimHolderRecycleDecision {
		return claimHolderRecycleDecision{
			Fire: true,
			Next: claimHolderRecycleState{Count: count, At: now, Claims: nextClaims},
		}
	}

	// No usable prior recycle — including a counter left without a timestamp,
	// which cannot be reasoned about — behaves exactly as today.
	if prior.Count < 1 || prior.At.IsZero() {
		return fire(1)
	}

	progressed := claimsKnown && claims != prior.Claims
	productive := lastActivity.Sub(prior.At) >= threshold
	if progressed || productive {
		return fire(1)
	}

	if wait := claimHolderRecycleBackoff(threshold, prior.Count); now.Sub(prior.At) < wait {
		return claimHolderRecycleDecision{
			Suppressed: true,
			Next:       prior,
			RetryAfter: prior.At.Add(wait),
		}
	}
	return fire(prior.Count + 1)
}

// claimHolderRecycleStateFromInfo projects the damper's accounting off a
// session snapshot. Unparsable markers read as "no state", which fails open to
// the undamped behavior rather than wedging on a corrupt value.
func claimHolderRecycleStateFromInfo(info sessionpkg.Info) claimHolderRecycleState {
	state := claimHolderRecycleState{Claims: info.ClaimHolderRecycleClaims}
	if count, err := strconv.Atoi(strings.TrimSpace(info.ClaimHolderRecycleCount)); err == nil && count > 0 {
		state.Count = count
	}
	if at, err := time.Parse(time.RFC3339, strings.TrimSpace(info.ClaimHolderRecycleAt)); err == nil {
		state.At = at.UTC()
	}
	return state
}

// patch renders the damper's accounting as a session-bead metadata write.
func (s claimHolderRecycleState) patch() sessionpkg.MetadataPatch {
	return sessionpkg.MetadataPatch{
		claimHolderRecycleCountKey:  strconv.Itoa(s.Count),
		claimHolderRecycleAtKey:     s.At.UTC().Format(time.RFC3339),
		claimHolderRecycleClaimsKey: s.Claims,
	}
}

// sessionInProgressClaimFingerprint returns a stable fingerprint of the
// in-progress work beads assigned to a session, across the same stores and
// identifiers the claim check itself consults. It answers "is this the same
// held work as last time?" without keeping a growing list of bead IDs on the
// session bead.
//
// It deliberately does NOT short-circuit the way the boolean claim check does:
// the whole point is the full set. It runs only for a session that has already
// tripped the stall gate, so the extra scan is rare.
func sessionInProgressClaimFingerprint(
	store beads.Store,
	rigStores map[string]beads.Store,
	info sessionpkg.Info,
	cfg *config.City,
) (string, error) {
	identifiers := sessionAssignmentIdentifiersForConfigInfo(info, cfg)
	ids := make(map[string]struct{})
	stores := make([]beads.Store, 0, 1+len(rigStores))
	stores = append(stores, store)
	for _, rs := range rigStores {
		stores = append(stores, rs)
	}
	for _, s := range stores {
		if s == nil {
			continue
		}
		wa := workAssignmentForStore(beads.WorkStore{Store: s})
		for _, assignee := range identifiers {
			if assignee == "" {
				continue
			}
			for _, tier := range []beads.TierMode{beads.TierIssues, beads.TierWisps} {
				items, err := wa.OpenAssignedTo(assignee, "in_progress", tier, true)
				if err != nil {
					return "", err
				}
				for _, id := range wa.NonSessionWorkIDs(items) {
					ids[id] = struct{}{}
				}
			}
		}
	}
	return claimFingerprint(ids), nil
}

// claimFingerprint renders a set of held bead IDs as a short, order-independent
// fingerprint. The claim count is kept in the clear so an operator reading the
// session bead can tell a one-bead holder from a five-bead one.
func claimFingerprint(ids map[string]struct{}) string {
	if len(ids) == 0 {
		return "0:"
	}
	sorted := make([]string, 0, len(ids))
	for id := range ids {
		sorted = append(sorted, id)
	}
	sort.Strings(sorted)
	sum := sha256.Sum256([]byte(strings.Join(sorted, "\x00")))
	return fmt.Sprintf("%d:%s", len(sorted), hex.EncodeToString(sum[:])[:16])
}
