package main

import (
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
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
	wantClaims := claimFingerprint(map[string]struct{}{work.ID: {}})
	if got.Metadata[claimHolderRecycleClaimsKey] != wantClaims {
		t.Fatalf("%s = %q, want %q", claimHolderRecycleClaimsKey, got.Metadata[claimHolderRecycleClaimsKey], wantClaims)
	}
	if got.Metadata[claimHolderRecycleAtKey] != env.clk.Now().UTC().Format(time.RFC3339) {
		t.Fatalf("%s = %q, want the recycle timestamp %q", claimHolderRecycleAtKey, got.Metadata[claimHolderRecycleAtKey], env.clk.Now().UTC().Format(time.RFC3339))
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
		claimHolderRecycleClaimsKey: claimFingerprint(map[string]struct{}{work.ID: {}}),
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
		claimHolderRecycleClaimsKey: claimFingerprint(map[string]struct{}{work.ID: {}}),
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
	if !strings.Contains(env.stderr.String(), "claim-holder recycle #2 changed neither its held claims nor its activity; next recycle not before "+wantNext) {
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
		claimHolderRecycleClaimsKey: claimFingerprint(map[string]struct{}{"some-earlier-bead": {}}),
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
	wantClaims := claimFingerprint(map[string]struct{}{work.ID: {}})
	if got.Metadata[claimHolderRecycleClaimsKey] != wantClaims {
		t.Fatalf("%s = %q, want the currently held claims %q", claimHolderRecycleClaimsKey, got.Metadata[claimHolderRecycleClaimsKey], wantClaims)
	}
}
