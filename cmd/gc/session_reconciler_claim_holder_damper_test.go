package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// claimHolderDamperEnv builds the shared fixture for the damper's reconciler
// tests: the claim-holder recycler ON, the claim-less recycler OFF, and one
// in-progress bead claimed by a session that has gone quiet past the threshold.
func claimHolderDamperEnv(t *testing.T) (*restartRequestTestEnv, beads.Bead, string, beads.Bead) {
	t.Helper()

	env, session, sessionName := newProgressStallTestEnv(t)
	env.cfg.Session.ProgressStallTimeout = ""       // claim-less recycler OFF
	env.cfg.Session.ClaimHolderStallTimeout = "20m" // claim-holder recycler ON

	work, err := env.store.Create(beads.Bead{Title: "claimed work", Type: "task", Assignee: sessionName})
	if err != nil {
		t.Fatalf("Create(work): %v", err)
	}
	status := "in_progress"
	if err := env.store.Update(work.ID, beads.UpdateOpts{Status: &status}); err != nil {
		t.Fatalf("Update(work): %v", err)
	}
	return env, session, sessionName, work
}

// TestReconcileSessionBeads_ClaimHolderRecycleStampsDamperState is the first
// half of the damper contract: the FIRST recycle is unchanged — it still fires
// — but it now leaves durable accounting on the session bead so the next one
// can be judged against it.
func TestReconcileSessionBeads_ClaimHolderRecycleStampsDamperState(t *testing.T) {
	env, session, sessionName, work := claimHolderDamperEnv(t)

	env.reconcileAtPath(t.TempDir(), []beads.Bead{session})

	if env.sp.IsRunning(sessionName) {
		t.Fatalf("session %q still running; the first wedged-holder recycle must still fire", sessionName)
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", session.ID, err)
	}
	if got.Metadata[claimHolderRecycleCountKey] != "1" {
		t.Fatalf("%s = %q, want \"1\"", claimHolderRecycleCountKey, got.Metadata[claimHolderRecycleCountKey])
	}
	wantClaims := claimFingerprint([]string{claimScopeCityStore}, map[string]struct{}{work.ID: {}})
	if got.Metadata[claimHolderRecycleClaimsKey] != wantClaims {
		t.Fatalf("%s = %q, want %q", claimHolderRecycleClaimsKey, got.Metadata[claimHolderRecycleClaimsKey], wantClaims)
	}
	if got.Metadata[claimHolderRecycleAtKey] != env.clk.Now().UTC().Format(time.RFC3339) {
		t.Fatalf("%s = %q, want the recycle timestamp %q", claimHolderRecycleAtKey, got.Metadata[claimHolderRecycleAtKey], env.clk.Now().UTC().Format(time.RFC3339))
	}
}

// restartSessionAfterRecycle puts the session back the way the controller does
// after a claim-holder recycle: the runtime comes back up and the restarted
// agent emits its startup burst, which in the observed incident landed 74-98
// seconds after every firing. It is the burst that makes "activity advanced"
// useless as a reset signal, so a durability test has to reproduce it.
func (e *restartRequestTestEnv) restartSessionAfterRecycle(t *testing.T, sessionName, sessionID string) {
	t.Helper()
	if err := e.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("restart session: %v", err)
	}
	if err := e.sp.SetMeta(sessionName, "GC_SESSION_ID", sessionID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}
	e.sp.SetActivity(sessionName, e.clk.Now().Add(79*time.Second))
}

// reloadSessionBead re-reads the session bead from the store so the next tick
// starts from PERSISTED state rather than an in-memory value.
func (e *restartRequestTestEnv) reloadSessionBead(t *testing.T, id string) beads.Bead {
	t.Helper()
	got, err := e.store.Get(id)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", id, err)
	}
	return got
}

// TestReconcileSessionBeads_ClaimHolderRecycleStateSurvivesARealRecycle is the
// load-bearing test for the whole fix. Every other test here hand-seeds the
// damper's prior state; this one never does. It drives three real reconciler
// ticks and re-reads the session bead from the store between each, so the
// accounting has to survive the reconciler's OWN recycle — including the
// RestartRequestPatch write that clears started_config_hash, last_woke_at and
// session_key — to be there for the next decision.
//
// That is the fix's central claim, and the reason the counter lives on the
// session bead at all: the restart handoff patches that bead in place rather
// than replacing it. If a recycle ever did replace the bead, the counter would
// reset every time and the damper would be decorative. This test fails loudly
// in that world.
func TestReconcileSessionBeads_ClaimHolderRecycleStateSurvivesARealRecycle(t *testing.T) {
	env, session, sessionName, work := claimHolderDamperEnv(t)
	cityPath := t.TempDir()
	wantClaims := claimFingerprint([]string{claimScopeCityStore}, map[string]struct{}{work.ID: {}})

	// Tick 1: the first recycle of a wedged holder. Unchanged behavior.
	env.reconcileAtPath(cityPath, []beads.Bead{session})
	if env.sp.IsRunning(sessionName) {
		t.Fatal("tick 1: the first wedged-holder recycle must still fire")
	}
	firstRecycleAt := env.clk.Now()
	after := env.reloadSessionBead(t, session.ID)
	if after.Metadata[claimHolderRecycleCountKey] != "1" || after.Metadata[claimHolderRecycleClaimsKey] != wantClaims {
		t.Fatalf("tick 1: damper state = %q/%q, want \"1\"/%q",
			after.Metadata[claimHolderRecycleCountKey], after.Metadata[claimHolderRecycleClaimsKey], wantClaims)
	}

	// The session restarts and emits its startup burst, then wedges again on
	// the same bead — the exact shape of the observed incident.
	env.restartSessionAfterRecycle(t, sessionName, session.ID)
	// Past the 20m stall threshold, but inside the 40m backoff tick 1 earned.
	env.clk.Time = env.clk.Time.Add(31 * time.Minute)

	// Tick 2: the repeat that ran 55 times in production.
	env.stderr.Reset()
	env.reconcileAtPath(cityPath, []beads.Bead{env.reloadSessionBead(t, session.ID)})
	if !env.sp.IsRunning(sessionName) {
		t.Fatal("tick 2: an ineffective repeat must be suppressed — this is the defect")
	}
	after = env.reloadSessionBead(t, session.ID)
	if after.Metadata[claimHolderRecycleCountKey] != "1" {
		t.Fatalf("tick 2: count = %q, want it held at \"1\" while suppressed", after.Metadata[claimHolderRecycleCountKey])
	}
	if after.Metadata[claimHolderRecycleAtKey] != firstRecycleAt.UTC().Format(time.RFC3339) {
		t.Fatalf("tick 2: stamp = %q, want the ORIGINAL %q — refreshing it would push the backoff out forever",
			after.Metadata[claimHolderRecycleAtKey], firstRecycleAt.UTC().Format(time.RFC3339))
	}
	if strings.Contains(env.stderr.String(), "requesting fresh restart") {
		t.Fatalf("tick 2: stderr = %q, want no restart request", env.stderr.String())
	}

	// Tick 3: once the backoff has elapsed the damper resumes. It is a backoff,
	// not a cap — a holder wedged on provider capacity that returns tomorrow
	// must still be recycled tomorrow.
	env.clk.Time = firstRecycleAt.Add(claimHolderRecycleBackoff(20*time.Minute, 1)).Add(time.Minute)
	env.stderr.Reset()
	env.reconcileAtPath(cityPath, []beads.Bead{env.reloadSessionBead(t, session.ID)})
	if env.sp.IsRunning(sessionName) {
		t.Fatal("tick 3: the damper must resume recycling once the backoff elapses")
	}
	after = env.reloadSessionBead(t, session.ID)
	if after.Metadata[claimHolderRecycleCountKey] != "2" {
		t.Fatalf("tick 3: count = %q, want \"2\" — the ineffective repeat accrues across the real recycle",
			after.Metadata[claimHolderRecycleCountKey])
	}
	if !strings.Contains(env.stderr.String(), "claim-holder recycle #2 changed neither its held claims nor its activity") {
		t.Fatalf("tick 3: stderr = %q, want the accrual announced once", env.stderr.String())
	}
}

// TestReconcileSessionBeads_ClaimHolderRecycleSuppressesIneffectiveRepeat is
// the defect itself: a session whose previous recycle changed neither its held
// claims nor its activity must NOT be recycled again on the next tick. This is
// the case that ran 55 times over four days.
func TestReconcileSessionBeads_ClaimHolderRecycleSuppressesIneffectiveRepeat(t *testing.T) {
	env, session, sessionName, work := claimHolderDamperEnv(t)

	// The previous recycle fired 31 minutes ago and bought nothing: the same
	// bead is still held, and the only activity since was the restart's own
	// startup burst a minute later.
	lastRecycle := env.clk.Now().Add(-31 * time.Minute)
	env.setSessionMetadata(&session, map[string]string{
		claimHolderRecycleCountKey:  "1",
		claimHolderRecycleAtKey:     lastRecycle.UTC().Format(time.RFC3339),
		claimHolderRecycleClaimsKey: claimFingerprint([]string{claimScopeCityStore}, map[string]struct{}{work.ID: {}}),
	})
	env.sp.SetActivity(sessionName, lastRecycle.Add(time.Minute))

	env.reconcileAtPath(t.TempDir(), []beads.Bead{session})

	if !env.sp.IsRunning(sessionName) {
		t.Fatalf("session %q was recycled again; an ineffective repeat must be suppressed", sessionName)
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", session.ID, err)
	}
	if got.Metadata["continuation_reset_pending"] == "true" {
		t.Fatalf("continuation_reset_pending = true; the restart handoff must not have run")
	}
	if got.Metadata[claimHolderRecycleCountKey] != "1" {
		t.Fatalf("%s = %q, want it left at \"1\" while suppressed", claimHolderRecycleCountKey, got.Metadata[claimHolderRecycleCountKey])
	}
	if got.Metadata[claimHolderRecycleAtKey] != lastRecycle.UTC().Format(time.RFC3339) {
		t.Fatalf("%s = %q, want the ORIGINAL stamp %q — refreshing it would push the backoff out forever", claimHolderRecycleAtKey, got.Metadata[claimHolderRecycleAtKey], lastRecycle.UTC().Format(time.RFC3339))
	}
	// Silence is the contract on a suppressed tick: the stall predicate stays
	// true for the whole backoff window, so a per-tick diagnostic here would be
	// thousands of lines a day. The accrual is announced once, on the tick that
	// records it (asserted in the resume test below).
	if strings.Contains(env.stderr.String(), "requesting fresh restart") {
		t.Fatalf("stderr = %q, want no restart request", env.stderr.String())
	}
}

// TestReconcileSessionBeads_ClaimHolderRecycleResumesAfterBackoff proves the
// damper is a backoff and not a hard cap: once the wait has elapsed, a still
// wedged holder is recycled again and the counter accrues.
func TestReconcileSessionBeads_ClaimHolderRecycleResumesAfterBackoff(t *testing.T) {
	env, session, sessionName, work := claimHolderDamperEnv(t)

	// One prior ineffective recycle at a 20m threshold means a 40m wait; this
	// one is three hours old, so the next attempt is due.
	lastRecycle := env.clk.Now().Add(-3 * time.Hour)
	env.setSessionMetadata(&session, map[string]string{
		claimHolderRecycleCountKey:  "1",
		claimHolderRecycleAtKey:     lastRecycle.UTC().Format(time.RFC3339),
		claimHolderRecycleClaimsKey: claimFingerprint([]string{claimScopeCityStore}, map[string]struct{}{work.ID: {}}),
	})
	env.sp.SetActivity(sessionName, lastRecycle.Add(time.Minute))

	env.reconcileAtPath(t.TempDir(), []beads.Bead{session})

	if env.sp.IsRunning(sessionName) {
		t.Fatalf("session %q still running; the damper must resume recycling once the backoff elapses", sessionName)
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", session.ID, err)
	}
	if got.Metadata[claimHolderRecycleCountKey] != "2" {
		t.Fatalf("%s = %q, want \"2\" — the repeat accrues", claimHolderRecycleCountKey, got.Metadata[claimHolderRecycleCountKey])
	}
	// The accrual announces itself once, here, with the next attempt's due
	// time — the whole story in one line per backoff window.
	wantNext := env.clk.Now().Add(claimHolderRecycleBackoff(20*time.Minute, 2)).UTC().Format(time.RFC3339)
	if !strings.Contains(env.stderr.String(), "claim-holder recycle #2 changed neither its held claims nor its activity; retrying now, then not again before "+wantNext) {
		t.Fatalf("stderr = %q, want the accrual diagnostic naming the next attempt at %s", env.stderr.String(), wantNext)
	}
}

// TestReconcileSessionBeads_ClaimHolderRecycleResetsWhenClaimsChange proves the
// counter tracks INEFFECTIVE recycles only: when the previous recycle moved the
// work on, the session starts over with a full allowance rather than inheriting
// a backoff it did not earn.
func TestReconcileSessionBeads_ClaimHolderRecycleResetsWhenClaimsChange(t *testing.T) {
	env, session, sessionName, work := claimHolderDamperEnv(t)

	lastRecycle := env.clk.Now().Add(-31 * time.Minute)
	env.setSessionMetadata(&session, map[string]string{
		claimHolderRecycleCountKey: "4",
		claimHolderRecycleAtKey:    lastRecycle.UTC().Format(time.RFC3339),
		// The session held a DIFFERENT bead at the last recycle; the one it
		// holds now is proof the recycle moved the work on.
		claimHolderRecycleClaimsKey: claimFingerprint([]string{claimScopeCityStore}, map[string]struct{}{"some-earlier-bead": {}}),
	})
	env.sp.SetActivity(sessionName, lastRecycle.Add(time.Minute))

	env.reconcileAtPath(t.TempDir(), []beads.Bead{session})

	if env.sp.IsRunning(sessionName) {
		t.Fatalf("session %q still running; a holder whose work moved on must not inherit a backoff", sessionName)
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", session.ID, err)
	}
	if got.Metadata[claimHolderRecycleCountKey] != "1" {
		t.Fatalf("%s = %q, want it reset to \"1\"", claimHolderRecycleCountKey, got.Metadata[claimHolderRecycleCountKey])
	}
	wantClaims := claimFingerprint([]string{claimScopeCityStore}, map[string]struct{}{work.ID: {}})
	if got.Metadata[claimHolderRecycleClaimsKey] != wantClaims {
		t.Fatalf("%s = %q, want the currently held claims %q", claimHolderRecycleClaimsKey, got.Metadata[claimHolderRecycleClaimsKey], wantClaims)
	}
}
