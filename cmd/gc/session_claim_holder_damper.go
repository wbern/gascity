package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
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

	// claimHolderRecycleBackoffCap is the longest a session is left un-recycled
	// on account of the damper. A day is long enough that a repeating
	// ineffective restart stops being a source of churn, and short enough that a
	// genuinely wedged worker is retried within an operator's normal horizon.
	claimHolderRecycleBackoffCap = 24 * time.Hour
)

// claimHolderRecycleDamper is the reconciler-facing face of this file: it holds
// the one tick's collaborators so the call site in session_reconciler.go stays a
// single `if`, wrapping upstream's two unchanged lines. Keeping the judgement,
// the durable write and the diagnostics here rather than inline is what keeps
// the fork's edit to that upstream file small enough to rebase without thought.
type claimHolderRecycleDamper struct {
	store     beads.Store
	rigStores map[string]beads.Store
	cfg       *config.City
	threshold time.Duration
	tick      *reconcileTick
	sessFront *sessionpkg.Store
	stderr    io.Writer
	trace     *SessionReconcilerTraceCycle
	template  string
}

// admitRecycle reports whether a session the stall predicate has already
// condemned should actually be recycled on this tick, and persists whatever
// accounting the decision produced. A false return means the recycle is being
// withheld because repeating it has been achieving nothing.
func (d claimHolderRecycleDamper) admitRecycle(id, name string, lastActivity, now time.Time) bool {
	info := d.tick.infoByID[id]
	claims, claimsErr := sessionInProgressClaimFingerprint(d.store, d.rigStores, info, d.cfg)
	decision := decideClaimHolderRecycle(
		claimHolderRecycleStateFromInfo(info),
		d.threshold,
		claims,
		claimsErr == nil,
		lastActivity,
		now,
	)
	if claimsErr != nil && decision.Fire {
		// Gated on Fire for the same reason the accrual diagnostic below is:
		// the stall predicate holds for the whole backoff window, so an ungated
		// line here would be ~2,600 a day at the measured tick cadence.
		// Suppressed ticks carry it in the trace instead.
		fmt.Fprintf(d.stderr, "session reconciler: fingerprinting held claims before claim-holder recycle for %s: %v\n", name, claimsErr) //nolint:errcheck
	}
	if decision.Persist {
		// applyStore, not apply: the accounting has to be DURABLE to outlive
		// the recycle it is counting. A failed write leaves the snapshot
		// unadvanced and the next tick simply behaves as it does today.
		//
		// This is deliberately written BEFORE the restart is known to have
		// executed. The alternative — stamp only after a confirmed kill —
		// loses the accounting exactly when the controller dies mid-restart,
		// which is the unbounded-repeat failure this damper exists to stop.
		// The cost is that a restart the reconciler later declines (a
		// kill-protected pinned session) or fails to perform still accrues;
		// both are cases where repeating the attempt every threshold achieves
		// nothing either.
		d.tick.applyStore(id, d.sessFront, decision.Next.patch())
	}
	if !decision.Fire {
		if d.trace != nil {
			d.trace.RecordDecision(TraceSiteReconcilerClaimHolderRecycle, TraceReasonClaimHolderRecycleIneffective, TraceOutcomeSuppressed, d.template, name, traceRecordPayload{
				"ineffective_recycles": decision.Next.Count,
				"retry_after":          decision.RetryAfter.UTC().Format(time.RFC3339),
				"claims_unreadable":    claimsErr != nil,
			})
		}
		return false
	}
	if decision.Next.Count > 1 {
		// Said once here, on the tick that accrues, rather than on every
		// suppressed tick after it: the stall predicate stays true for the
		// whole backoff window, so a per-tick line would be ~2,600 a day.
		//
		// It precedes the caller's "requesting fresh restart" line, so it is
		// worded as the reason for the restart that follows rather than as a
		// deferral of it.
		fmt.Fprintf(d.stderr, "session reconciler: %s claim-holder recycle #%d changed neither its held claims nor its activity; retrying now, then not again before %s\n", name, decision.Next.Count, decision.Next.At.Add(claimHolderRecycleBackoff(d.threshold, decision.Next.Count)).UTC().Format(time.RFC3339)) //nolint:errcheck
	}
	return true
}

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
//
// Persist is separate from Fire because the damper has one outcome that writes
// without acting: repairing a stamp it cannot time (see decideClaimHolderRecycle).
type claimHolderRecycleDecision struct {
	Fire       bool
	Suppressed bool
	Persist    bool
	Next       claimHolderRecycleState
	RetryAfter time.Time
}

// claimHolderRecycleBackoff returns how long to wait after a recycle before
// repeating it, given how many consecutive recycles have already changed
// nothing. It doubles per ineffective repeat, clamped to
// claimHolderRecycleBackoffCap. A threshold at or beyond the cap is its own
// backoff — a city that already waits that long for a stall should not have the
// damper shorten it.
//
// The doubling is a loop rather than a shift by count so the ceiling is the CAP
// for every threshold. A shift bounded by a fixed maximum reaches 24h only for
// thresholds near it: at the 30m the incident ran with, six doublings clear the
// cap, but a city configured at 5m would top out at 5h20m and keep recycling a
// wedged holder four times a day forever. The loop cannot overflow either — it
// stops the moment the value reaches the cap, so it never doubles a duration
// larger than 24h.
func claimHolderRecycleBackoff(threshold time.Duration, count int) time.Duration {
	if threshold <= 0 || count < 1 {
		return 0
	}
	if threshold >= claimHolderRecycleBackoffCap {
		return threshold
	}
	backoff := threshold
	for i := 0; i < count && backoff < claimHolderRecycleBackoffCap; i++ {
		backoff *= 2
	}
	if backoff > claimHolderRecycleBackoffCap {
		return claimHolderRecycleBackoffCap
	}
	return backoff
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
			Fire:    true,
			Persist: true,
			Next:    claimHolderRecycleState{Count: count, At: now, Claims: nextClaims},
		}
	}

	// No usable prior recycle — including a counter left without a timestamp,
	// which cannot be reasoned about — behaves exactly as today.
	if prior.Count < 1 || prior.At.IsZero() {
		return fire(1)
	}

	// A stamp in the FUTURE is the one unusable marker that would fail closed:
	// it parses, so it is not caught above, but every elapsed-time test below
	// reads negative and suppresses. Left alone the session is never recycled
	// again until wall-clock catches up — for a hand-edited or corrupt stamp,
	// never. Firing instead would be an overcorrection in the other direction:
	// a backward NTP step of a couple of seconds would cost a session holding
	// in-progress work an extra destructive restart. So withhold the recycle
	// but re-base the stamp on now, which bounds the suppression at one backoff
	// window and heals the marker on the way through.
	if prior.At.After(now) {
		repaired := prior
		repaired.At = now
		repaired.Claims = nextClaims
		return claimHolderRecycleDecision{
			Suppressed: true,
			Persist:    true,
			Next:       repaired,
			RetryAfter: now.Add(claimHolderRecycleBackoff(threshold, prior.Count)),
		}
	}

	progressed := claimsKnown && claimsComparable(claims, prior.Claims) && claims != prior.Claims
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
	// Trimmed like its two siblings: the damper only ever writes a fingerprint
	// with no surrounding space, so a padded value is a hand edit. Comparing it
	// raw would make it differ from every freshly computed fingerprint, read as
	// perpetual progress, and reset the counter on every tick — the original
	// unbounded loop, restored by a stray space.
	state := claimHolderRecycleState{Claims: strings.TrimSpace(info.ClaimHolderRecycleClaims)}
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
	scope := make([]string, 0, 1+len(rigStores))
	stores := make([]beads.Store, 0, 1+len(rigStores))
	if store != nil {
		scope = append(scope, claimScopeCityStore)
		stores = append(stores, store)
	}
	for name, rs := range rigStores {
		if rs == nil {
			continue
		}
		scope = append(scope, claimScopeRigStorePrefix+name)
		stores = append(stores, rs)
	}
	for _, s := range stores {
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
	return claimFingerprint(scope, ids), nil
}

const (
	// claimScopeCityStore and claimScopeRigStorePrefix name the stores a
	// fingerprint was read from. They are prefixed so a rig literally named
	// "city" cannot collide with the city store's own entry.
	claimScopeCityStore      = "@city"
	claimScopeRigStorePrefix = "@rig/"
)

// claimFingerprint renders a set of held bead IDs as a short, order-independent
// fingerprint of the form "<scope>:<count>:<digest>".
//
// The claim count is kept in the clear so an operator reading the session bead
// can tell a one-bead holder from a five-bead one.
//
// The leading SCOPE digest — over the set of stores the IDs were read from — is
// what makes the damper's progress signal trustworthy. rigStores is rebuilt
// wholesale on every config reload (controllerState.buildStores), and it SKIPS
// any rig that is unbound or whose store fails to open, so the set of stores
// the reconciler can see genuinely varies between ticks. Without the scope, a
// rig dropping out would shrink the claim set, read as "the held work moved on"
// and reset the damper — silently restoring the unbounded loop this file
// exists to stop, in exactly the transient-store conditions where a wedged
// holder is most likely. Fingerprints from different scopes are not compared
// (see claimsComparable); the damper falls back to its activity-window signal,
// which is the same conservative path an unreadable claim set takes.
func claimFingerprint(scope []string, ids map[string]struct{}) string {
	sortedScope := append([]string(nil), scope...)
	sort.Strings(sortedScope)
	scopeSum := sha256.Sum256([]byte(strings.Join(sortedScope, "\x00")))
	prefix := hex.EncodeToString(scopeSum[:])[:8]
	if len(ids) == 0 {
		return prefix + ":0:"
	}
	sorted := make([]string, 0, len(ids))
	for id := range ids {
		sorted = append(sorted, id)
	}
	sort.Strings(sorted)
	sum := sha256.Sum256([]byte(strings.Join(sorted, "\x00")))
	return fmt.Sprintf("%s:%d:%s", prefix, len(sorted), hex.EncodeToString(sum[:])[:16])
}

// claimsComparable reports whether two fingerprints were read over the same set
// of stores, and so whether a difference between them is evidence that the held
// work moved rather than evidence that the reconciler's view of the stores did.
// A fingerprint without a scope (empty, or a stored marker predating this
// format) is comparable only with another of its own kind.
func claimsComparable(a, b string) bool {
	return claimFingerprintScope(a) == claimFingerprintScope(b)
}

// claimFingerprintScope returns the scope digest a fingerprint was built with,
// or "" if the value does not carry one.
func claimFingerprintScope(fingerprint string) string {
	scope, rest, ok := strings.Cut(fingerprint, ":")
	if !ok || !strings.Contains(rest, ":") {
		return ""
	}
	return scope
}
