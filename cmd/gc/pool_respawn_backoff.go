package main

import (
	"fmt"
	"hash/fnv"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// poolRespawnBackoffConfig controls the jittered exponential backoff applied to
// pool respawns after a spawned generic session drains without ever claiming its
// trigger work (upstream #3279 respawn storm). A base of zero disables the
// mechanism entirely, so the feature lands dormant and is enabled by config.
type poolRespawnBackoffConfig struct {
	base time.Duration // first backoff window; 0 disables the mechanism
	max  time.Duration // cap on the (pre-jitter) window
}

// enabled reports whether the backoff mechanism is active.
func (c poolRespawnBackoffConfig) enabled() bool {
	return c.base > 0
}

// poolRespawnBackoffEntry is the per-template backoff state.
type poolRespawnBackoffEntry struct {
	consecutive int       // consecutive no-claim drains observed for this template
	until       time.Time // fresh pool creates for this template are deferred until this instant
	lastDrainAt time.Time // when the most recent *new* no-claim drain was observed
}

// poolRespawnBackoffTracker throttles the rate at which the pool recreates a
// generic session for a template after consecutive no-claim drains. It is
// clock-aware, cross-tick state captured by the reconcile-tick closure,
// mirroring the order-dispatch gate-backoff (order_dispatch.go). It observes
// each *new* no-wake-reason drain — edge-detected by session bead ID so a single
// lingering drained bead cannot ramp the window — and arms an exponentially
// growing, deterministically jittered deadline per template.
//
// Recovery is purely time-based: the exponent decays back to the base window
// after a quiet gap (see resetQuiet). There is deliberately NO claim/success
// observation — a freshly spawned session is "active" while it does its worktree
// checkout, which is indistinguishable from a genuinely working session without
// reading its work beads, so a liveness-based reset would falsely clear the
// exponent every storm iteration. The trade-off is that while a template is
// backed off, even legitimate new demand for it waits up to the current window;
// that is the intended cost of throttling a template that cannot make progress.
//
// This is pure respawn-rate throttling: it makes no judgment about the work
// itself, only about how fast a repeatedly-failing spawn is retried. That keeps
// it framework transport (like health-patrol and order-gate backoff), not
// framework cognition.
type poolRespawnBackoffTracker struct {
	mu      sync.Mutex
	cfg     poolRespawnBackoffConfig
	entries map[string]*poolRespawnBackoffEntry // template -> backoff state
	seen    map[string]bool                     // drained session bead ID -> already counted
}

// newPoolRespawnBackoffTracker constructs a tracker with the given config.
func newPoolRespawnBackoffTracker(cfg poolRespawnBackoffConfig) *poolRespawnBackoffTracker {
	return &poolRespawnBackoffTracker{
		cfg:     cfg,
		entries: make(map[string]*poolRespawnBackoffEntry),
		seen:    make(map[string]bool),
	}
}

// setConfig updates the backoff config. The reconcile tick calls it once per
// build from the live city config so a config reload takes effect without
// reconstructing the (stateful) tracker. Guarded so cfg reads stay race-free.
func (t *poolRespawnBackoffTracker) setConfig(cfg poolRespawnBackoffConfig) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cfg = cfg
}

// enabled reports whether the mechanism is active, reading cfg under the lock.
func (t *poolRespawnBackoffTracker) enabled() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cfg.enabled()
}

// resetQuiet is the no-new-drain gap after which the exponent decays back to
// zero, so a template that has genuinely recovered restarts from the base window
// rather than staying penalized indefinitely.
//
// Its correctness has a documented coupling to worktree-checkout time. While a
// storm is throttled, successive no-claim drains are spaced by roughly
// (current window + checkout time C): the window defers the respawn, then the
// respawn spends C seconds checking out before it drains again. Decay must not
// fire on that spacing, so resetQuiet (2*max) must exceed (max + C), i.e. the
// backoff cap `max` must be set above the pool's worktree-checkout time. When it
// is (the default max=5m comfortably exceeds a typical checkout), an ongoing
// storm keeps re-arming and never decays; only true quiet does. If an operator
// sets `max` at or below checkout time, decay can fire mid-storm and the window
// sawtooths between base and cap instead of holding at the cap — the storm is
// still bounded to ~one respawn per base window, but the exponential ramp no
// longer persists. This is why PoolRespawnBackoffMaxDuration documents that the
// cap should exceed checkout time.
func (t *poolRespawnBackoffTracker) resetQuiet() time.Duration {
	if t.cfg.max > 0 {
		return 2 * t.cfg.max
	}
	return 2 * t.cfg.base
}

// observeNoClaimDrain records that the pool session identified by sessionBeadID
// (belonging to template) drained without claiming its trigger work. Only the
// first observation of a given session bead ID counts: re-seeing the same
// drained bead on later ticks is a no-op, so the exponent tracks real respawn
// iterations, not lingering beads. When a template has been quiet (no new
// no-claim drain) for longer than resetQuiet, the exponent decays to zero before
// this drain is counted, so a recovered-then-refailed pool restarts at the base
// window. Disabled when base==0.
//
// Edge detection assumes each real respawn mints a NEW session bead ID — true
// for the suffixed expanding pools that storm. A canonical-singleton-identity
// pool reuses the same bead ID across wake/re-drain cycles, so repeated no-wake
// drains on it count once and the exponent will not climb; that is harmless
// because singletons reuse rather than fresh-create, so they do not storm.
func (t *poolRespawnBackoffTracker) observeNoClaimDrain(template, sessionBeadID string, now time.Time) {
	if t == nil || template == "" || sessionBeadID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.cfg.enabled() {
		return
	}
	if t.seen[sessionBeadID] {
		return
	}
	t.seen[sessionBeadID] = true
	entry := t.entries[template]
	if entry == nil {
		entry = &poolRespawnBackoffEntry{}
		t.entries[template] = entry
	}
	if !entry.lastDrainAt.IsZero() && now.Sub(entry.lastDrainAt) >= t.resetQuiet() {
		entry.consecutive = 0
	}
	entry.consecutive++
	entry.until = now.Add(t.window(template, entry.consecutive))
	entry.lastDrainAt = now
}

// forgetAbsentDrains prunes the seen-set down to session bead IDs still present
// in liveIDs. It bounds memory across the lifetime of the process and lets a
// bead ID that was reaped and (improbably) reused be counted afresh.
func (t *poolRespawnBackoffTracker) forgetAbsentDrains(liveIDs map[string]bool) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for id := range t.seen {
		if !liveIDs[id] {
			delete(t.seen, id)
		}
	}
}

// observeSnapshot feeds the current session-bead snapshot into the tracker: it
// records every pool-managed session that has drained with reason
// "no-wake-reason" (a spawn that never claimed its trigger work) and prunes the
// seen-set down to the still-open session beads. Callers invoke it once per
// desired-state build, before consulting activeTemplates, so a drain observed
// this build immediately gates a respawn in the same build.
func (t *poolRespawnBackoffTracker) observeSnapshot(cfg *config.City, sessionBeads *sessionBeadSnapshot, now time.Time) {
	if t == nil || sessionBeads == nil || !t.enabled() {
		return
	}
	live := make(map[string]bool)
	for _, info := range sessionBeads.OpenInfos() {
		if info.ID == "" || info.Closed {
			continue
		}
		live[info.ID] = true
		if !isPoolManagedSessionInfo(info) {
			continue
		}
		// Only "no-wake-reason" arms the backoff: it is precisely "spawned but
		// found no work to claim", the storm signature. Other drain reasons must
		// NOT arm it — "idle" means the session did work and finished,
		// "provider-terminal-error" is a dead session, an explicit sleep intent is
		// deliberate — so throttling respawn on those would be wrong.
		if strings.TrimSpace(info.SleepReason) != string(sessionpkg.SleepReasonNoWakeReason) {
			continue
		}
		template := strings.TrimSpace(normalizedSessionTemplateInfo(info, cfg))
		if template == "" {
			continue
		}
		t.observeNoClaimDrain(template, info.ID, now)
	}
	t.forgetAbsentDrains(live)
}

// backedOff reports whether fresh pool creates for template must be deferred at
// the given instant.
func (t *poolRespawnBackoffTracker) backedOff(template string, now time.Time) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.cfg.enabled() {
		return false
	}
	entry := t.entries[template]
	return entry != nil && now.Before(entry.until)
}

// windowRemaining returns how long template stays deferred from now, or 0 when
// it is not currently backed off.
func (t *poolRespawnBackoffTracker) windowRemaining(template string, now time.Time) time.Duration {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.cfg.enabled() {
		return 0
	}
	entry := t.entries[template]
	if entry == nil || !now.Before(entry.until) {
		return 0
	}
	return entry.until.Sub(now)
}

// activeTemplates returns the set of templates currently deferred at now. The
// reconciler passes this into buildDesiredState so the fresh-create guard can
// refuse them, exactly as it refuses scale-check-partial templates.
func (t *poolRespawnBackoffTracker) activeTemplates(now time.Time) map[string]bool {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.cfg.enabled() {
		return nil
	}
	var active map[string]bool
	for template, entry := range t.entries {
		if entry != nil && now.Before(entry.until) {
			if active == nil {
				active = make(map[string]bool)
			}
			active[template] = true
		}
	}
	return active
}

// poolRespawnBackoffConfigFromCity reads the respawn-backoff knobs from the city
// config. Base zero (unset) disables the mechanism.
func poolRespawnBackoffConfigFromCity(cfg *config.City) poolRespawnBackoffConfig {
	if cfg == nil {
		return poolRespawnBackoffConfig{}
	}
	return poolRespawnBackoffConfig{
		base: cfg.Session.PoolRespawnBackoffBaseDuration(),
		max:  cfg.Session.PoolRespawnBackoffMaxDuration(),
	}
}

// applyPoolRespawnBackoffObservation refreshes the tracker's config from the live
// city config, feeds it the current session-bead snapshot (recording no-claim
// drains), and returns the set of templates currently under backoff for the
// desired-state build's fresh-create gate. Returns nil (gate disabled) when the
// tracker is nil or the mechanism is not configured. now is the observation
// instant; callers pass the wall clock in production.
func applyPoolRespawnBackoffObservation(tr *poolRespawnBackoffTracker, cfg *config.City, sessionBeads *sessionBeadSnapshot, now time.Time) map[string]bool {
	if tr == nil {
		return nil
	}
	tr.setConfig(poolRespawnBackoffConfigFromCity(cfg))
	tr.observeSnapshot(cfg, sessionBeads, now)
	return tr.activeTemplates(now)
}

// window computes the backoff duration for the nth consecutive no-claim drain:
// base doubled per step and capped at max (the exponential value w), plus a
// deterministic jitter in [0, w) derived from a hash of the template and step, so
// the effective window is in [w, 2w).
//
// The jitter spans a FULL window, not a fraction, because the storm's herd arms
// together: when the store breaker trips, many pools drain no-wake on the same
// tick and ramp to w=max in lockstep, so a narrow jitter would only spread their
// respawns across a sliver of the window and they would re-synchronize. A
// full-window span de-correlates them across the whole window. It deliberately
// keeps the exponential value w as a FLOOR (jitter is additive, never below w)
// rather than using AWS-style full jitter over [0, w]: this throttles a
// persistent failure (the breaker is still tripped), so preserving a minimum
// backoff matters more than the marginally better spread of a floorless jitter,
// which could let a respawn fire almost immediately. The hash keeps it RNG- and
// wallclock-free, hence reproducible and testable; per-template determinism is
// fine because different templates hash differently and a single template's own
// successive respawns are already separated in time by the window itself.
func (t *poolRespawnBackoffTracker) window(template string, consecutive int) time.Duration {
	if consecutive < 1 {
		consecutive = 1
	}
	base := t.cfg.base
	w := base
	for i := 1; i < consecutive; i++ {
		w *= 2
		if t.cfg.max > 0 && w >= t.cfg.max {
			w = t.cfg.max
			break
		}
	}
	if t.cfg.max > 0 && w > t.cfg.max {
		w = t.cfg.max
	}
	if w <= 0 {
		return w
	}
	h := fnv.New64a()
	// Errors from Hash.Write are documented to never occur; ignore explicitly.
	_, _ = fmt.Fprintf(h, "%s:%d", template, consecutive)
	offset := time.Duration(h.Sum64() % uint64(w))
	return w + offset
}
