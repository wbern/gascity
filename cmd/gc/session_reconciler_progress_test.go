package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

type assignedWorkListErrorStore struct {
	beads.Store
	err error
}

func (s *assignedWorkListErrorStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Assignee != "" && (query.Status == "open" || query.Status == "in_progress") {
		return nil, s.err
	}
	return s.Store.List(query)
}

type sessionObservationGetErrorStore struct {
	beads.Store
	id        string
	remaining int
	err       error
}

func (s *sessionObservationGetErrorStore) Get(id string) (beads.Bead, error) {
	if id == s.id && s.remaining > 0 {
		s.remaining--
		return beads.Bead{}, s.err
	}
	return s.Store.Get(id)
}

func newProgressStallTestEnv(t *testing.T) (*restartRequestTestEnv, beads.Bead, string) {
	t.Helper()

	env := newRestartRequestTestEnv()
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Session: config.SessionConfig{
			ProgressStallTimeout: "30m",
			StartupTimeout:       "60s",
		},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: restartRequestTestIntPtr(1)}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "on_demand"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.desiredState[sessionName] = TemplateParams{
		Command:      "true",
		SessionName:  sessionName,
		TemplateName: "worker",
		ResolvedProvider: &config.ResolvedProvider{
			Name:          "zai",
			SessionIDFlag: "--session-id",
		},
	}

	session := env.createSessionBead(sessionName)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "on_demand",
		"state":                      "active",
		"session_key":                "original-key",
		"started_config_hash":        "hash-before-restart",
	})
	if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	if err := env.sp.SetMeta(sessionName, "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}
	env.sp.SetActivity(sessionName, env.clk.Now().Add(-time.Hour))

	return env, session, sessionName
}

func (e *restartRequestTestEnv) reconcileAtPath(cityPath string, sessions []beads.Bead) {
	e.reconcileAtPathWithProvider(cityPath, e.sp, sessions)
}

// reconcileAtPathWithDrainOps is reconcileAtPathWithProvider with an injected
// drainOps, so a test can seed a controller drain-ack (dops.isDrainAcked) — the
// gate on the two finalizeDrainAckStoppedSession call sites that live below the
// drain-ack-stop-pending fast path (the orphan drain-ack close and the
// reconciler-owned drain-ack close). Everything else matches reconcileAtPath.
func (e *restartRequestTestEnv) reconcileAtPathWithDrainOps(cityPath string, sessions []beads.Bead, dops drainOps) {
	poolDesired := make(map[string]int)
	for _, tp := range e.desiredState {
		if tp.TemplateName != "" {
			poolDesired[tp.TemplateName]++
		}
	}
	cfgNames := configuredSessionNames(e.cfg, "", e.store)
	_ = reconcileSessionBeadsAtPath(
		context.Background(),
		cityPath,
		sessions,
		e.desiredState,
		cfgNames,
		e.cfg,
		e.sp,
		e.store,
		dops,
		nil,
		nil,
		nil,
		e.dt,
		poolDesired,
		false,
		nil,
		"",
		nil,
		e.clk,
		e.rec,
		0,
		0,
		&e.stdout,
		&e.stderr,
		e.startOptions...,
	)
}

func (e *restartRequestTestEnv) reconcileAtPathWithProvider(cityPath string, sp runtime.Provider, sessions []beads.Bead) {
	poolDesired := make(map[string]int)
	for _, tp := range e.desiredState {
		if tp.TemplateName != "" {
			poolDesired[tp.TemplateName]++
		}
	}
	cfgNames := configuredSessionNames(e.cfg, "", e.store)
	_ = reconcileSessionBeadsAtPath(
		context.Background(),
		cityPath,
		sessions,
		e.desiredState,
		cfgNames,
		e.cfg,
		sp,
		e.store,
		nil,
		nil,
		nil,
		nil,
		e.dt,
		poolDesired,
		false,
		nil,
		"",
		nil,
		e.clk,
		e.rec,
		0,
		0,
		&e.stdout,
		&e.stderr,
		e.startOptions...,
	)
}

func TestReconcileSessionBeads_ProgressStallRecyclesStaleClaimlessHealthySession(t *testing.T) {
	env, session, sessionName := newProgressStallTestEnv(t)

	env.reconcileAtPath(t.TempDir(), []beads.Bead{session})

	if env.sp.IsRunning(sessionName) {
		t.Fatalf("session %q still running; stale claim-less session should be recycled", sessionName)
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", session.ID, err)
	}
	if got.Metadata["restart_requested"] != "" {
		t.Fatalf("restart_requested = %q, want cleared after restart handoff", got.Metadata["restart_requested"])
	}
	if got.Metadata["continuation_reset_pending"] != "true" {
		t.Fatalf("continuation_reset_pending = %q, want true", got.Metadata["continuation_reset_pending"])
	}
	if !strings.Contains(env.stderr.String(), "progress-stalled") {
		t.Fatalf("stderr = %q, want progress-stalled diagnostic", env.stderr.String())
	}
}

func TestReconcileSessionBeads_ProgressStallRecyclesWithOpenAssignedWork(t *testing.T) {
	env, session, sessionName := newProgressStallTestEnv(t)
	work, err := env.store.Create(beads.Bead{
		Title:    "ready work not yet claimed",
		Type:     "task",
		Assignee: sessionName,
	})
	if err != nil {
		t.Fatalf("Create(work): %v", err)
	}

	env.reconcileAtPath(t.TempDir(), []beads.Bead{session})

	if env.sp.IsRunning(sessionName) {
		t.Fatalf("session %q still running; open assigned work is not a held claim", sessionName)
	}
	gotWork, err := env.store.Get(work.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", work.ID, err)
	}
	if gotWork.Status != "open" {
		t.Fatalf("work status = %q, want open", gotWork.Status)
	}
	if gotWork.Assignee != sessionName {
		t.Fatalf("work assignee = %q, want %q", gotWork.Assignee, sessionName)
	}
}

func TestReconcileSessionBeads_ProgressStallDoesNotRecycleExemptOrSafeSessions(t *testing.T) {
	tests := []struct {
		name      string
		cityPath  func(t *testing.T) string
		configure func(t *testing.T, env *restartRequestTestEnv, session *beads.Bead, sessionName string)
		provider  func(env *restartRequestTestEnv) runtime.Provider
		wantLog   string
	}{
		{
			name: "attached session",
			configure: func(_ *testing.T, env *restartRequestTestEnv, _ *beads.Bead, sessionName string) {
				env.sp.SetAttached(sessionName, true)
			},
		},
		{
			name: "claim check error fails safe",
			configure: func(_ *testing.T, env *restartRequestTestEnv, _ *beads.Bead, _ string) {
				env.store = &assignedWorkListErrorStore{Store: env.store, err: errors.New("assigned work query failed")}
			},
			wantLog: "checking assigned work before progress-stall recycle",
		},
		{
			name: "attachment check error fails safe",
			configure: func(_ *testing.T, env *restartRequestTestEnv, session *beads.Bead, _ string) {
				env.store = &sessionObservationGetErrorStore{
					Store:     env.store,
					id:        session.ID,
					remaining: 1,
					err:       errors.New("attachment observation failed"),
				}
			},
			wantLog: "checking attachment before progress-stall recycle",
		},
		{
			name: "in-progress assigned work",
			configure: func(t *testing.T, env *restartRequestTestEnv, _ *beads.Bead, sessionName string) {
				t.Helper()
				work, err := env.store.Create(beads.Bead{
					Title:    "claimed work",
					Type:     "task",
					Assignee: sessionName,
				})
				if err != nil {
					t.Fatalf("Create(work): %v", err)
				}
				status := "in_progress"
				if err := env.store.Update(work.ID, beads.UpdateOpts{Status: &status}); err != nil {
					t.Fatalf("Update(work): %v", err)
				}
			},
		},
		{
			name: "provider health red",
			cityPath: func(t *testing.T) string {
				dir := t.TempDir()
				writeHealthCache(t, dir, "zai", "unhealthy", nowSecs())
				return dir
			},
		},
		{
			name: "recent provider activity",
			configure: func(_ *testing.T, env *restartRequestTestEnv, _ *beads.Bead, sessionName string) {
				env.sp.SetActivity(sessionName, env.clk.Now().Add(-time.Minute))
			},
		},
		{
			name: "unknown provider activity fails safe",
			provider: func(env *restartRequestTestEnv) runtime.Provider {
				return capabilityOverrideProvider{
					Provider: env.sp,
					caps: runtime.ProviderCapabilities{
						CanReportAttachment: true,
						CanReportActivity:   false,
					},
					sleepCap: runtime.SessionSleepCapabilityTimedOnly,
				}
			},
		},
		{
			name: "startup in-flight lease",
			configure: func(_ *testing.T, env *restartRequestTestEnv, session *beads.Bead, _ string) {
				env.setSessionMetadata(session, map[string]string{
					"pending_create_claim": "true",
					"state":                string(sessionpkg.StateCreating),
					"last_woke_at":         env.clk.Now().UTC().Format(time.RFC3339),
				})
			},
		},
		{
			name: "timeout below enforced minimum",
			configure: func(_ *testing.T, env *restartRequestTestEnv, _ *beads.Bead, sessionName string) {
				env.cfg.Session.ProgressStallTimeout = "30s"
				env.sp.SetActivity(sessionName, env.clk.Now().Add(-time.Minute))
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env, session, sessionName := newProgressStallTestEnv(t)
			cityPath := t.TempDir()
			if tc.cityPath != nil {
				cityPath = tc.cityPath(t)
			}
			if tc.configure != nil {
				tc.configure(t, env, &session, sessionName)
			}
			sp := runtime.Provider(env.sp)
			if tc.provider != nil {
				sp = tc.provider(env)
			}

			env.reconcileAtPathWithProvider(cityPath, sp, []beads.Bead{session})

			if !env.sp.IsRunning(sessionName) {
				t.Fatalf("session %q was recycled; want it left running", sessionName)
			}
			got, err := env.store.Get(session.ID)
			if err != nil {
				t.Fatalf("store.Get(%s): %v", session.ID, err)
			}
			if got.Metadata["continuation_reset_pending"] != "" {
				t.Fatalf("continuation_reset_pending = %q, want empty", got.Metadata["continuation_reset_pending"])
			}
			if strings.Contains(env.stderr.String(), "progress-stalled") {
				t.Fatalf("stderr = %q, want no progress-stalled diagnostic", env.stderr.String())
			}
			if tc.wantLog != "" && !strings.Contains(env.stderr.String(), tc.wantLog) {
				t.Fatalf("stderr = %q, want %q", env.stderr.String(), tc.wantLog)
			}
		})
	}
}

// TestReconcileSessionBeads_ClaimHolderStallRecyclesWedgedHolder drives the
// claim-holder recycler end-to-end: a desired, alive session that HOLDS an
// in-progress claim but has gone stale (its turn ended on a non-self-clearing
// provider banner) is recycled when [session] claim_holder_stall_timeout is set —
// the case the claim-less recycler deliberately exempts and no other mechanism
// reaps (#4012). The claim-less timeout is disabled here to prove the claim-holder
// path fires on its own.
func TestReconcileSessionBeads_ClaimHolderStallRecyclesWedgedHolder(t *testing.T) {
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

	env.reconcileAtPath(t.TempDir(), []beads.Bead{session})

	if env.sp.IsRunning(sessionName) {
		t.Fatalf("session %q still running; wedged claim-holder should be recycled", sessionName)
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", session.ID, err)
	}
	if got.Metadata["continuation_reset_pending"] != "true" {
		t.Fatalf("continuation_reset_pending = %q, want true", got.Metadata["continuation_reset_pending"])
	}
	if !strings.Contains(env.stderr.String(), "claim-holder-stalled") {
		t.Fatalf("stderr = %q, want claim-holder-stalled diagnostic", env.stderr.String())
	}
}

// TestReconcileSessionBeads_ClaimHolderStallOffByDefaultKeepsHolder is the
// regression guard: with claim_holder_stall_timeout unset (the default), a stale
// claim-holder is left running exactly as before — the new path is strictly
// opt-in and does not change behavior for cities that have not enabled it.
func TestReconcileSessionBeads_ClaimHolderStallOffByDefaultKeepsHolder(t *testing.T) {
	env, session, sessionName := newProgressStallTestEnv(t)
	// ProgressStallTimeout stays "30m" (default env), ClaimHolderStallTimeout unset.
	work, err := env.store.Create(beads.Bead{Title: "claimed work", Type: "task", Assignee: sessionName})
	if err != nil {
		t.Fatalf("Create(work): %v", err)
	}
	status := "in_progress"
	if err := env.store.Update(work.ID, beads.UpdateOpts{Status: &status}); err != nil {
		t.Fatalf("Update(work): %v", err)
	}

	env.reconcileAtPath(t.TempDir(), []beads.Bead{session})

	if !env.sp.IsRunning(sessionName) {
		t.Fatalf("session %q recycled; claim-holder must be left running when claim_holder_stall_timeout is unset", sessionName)
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", session.ID, err)
	}
	if got.Metadata["continuation_reset_pending"] != "" {
		t.Fatalf("continuation_reset_pending = %q, want empty", got.Metadata["continuation_reset_pending"])
	}
	if strings.Contains(env.stderr.String(), "stalled") {
		t.Fatalf("stderr = %q, want no stall diagnostic", env.stderr.String())
	}
}

// TestReconcileSessionBeads_ClaimHolderStallRespectsLargerThresholdWithBothSet
// is the core safety-separation guard: with BOTH timeouts set and the
// claim-holder timeout deliberately larger, a claim-holder whose activity is
// past the (smaller) claim-less threshold but still within its own (larger)
// claim-holder threshold must be left running. This proves the minPositiveDuration
// gate opens the block for the holder yet the holder is protected until its own,
// more conservative deadline — the whole point of giving claim-holders a separate
// timeout, since recycling one discards in-progress work.
func TestReconcileSessionBeads_ClaimHolderStallRespectsLargerThresholdWithBothSet(t *testing.T) {
	env, session, sessionName := newProgressStallTestEnv(t)
	env.cfg.Session.ProgressStallTimeout = "30m"    // claim-less
	env.cfg.Session.ClaimHolderStallTimeout = "45m" // claim-holder, larger
	// 35m stale: past the 30m gate (so the block runs) but under the 45m
	// claim-holder threshold (so the holder must not be recycled).
	env.sp.SetActivity(sessionName, env.clk.Now().Add(-35*time.Minute))

	work, err := env.store.Create(beads.Bead{Title: "claimed work", Type: "task", Assignee: sessionName})
	if err != nil {
		t.Fatalf("Create(work): %v", err)
	}
	status := "in_progress"
	if err := env.store.Update(work.ID, beads.UpdateOpts{Status: &status}); err != nil {
		t.Fatalf("Update(work): %v", err)
	}

	env.reconcileAtPath(t.TempDir(), []beads.Bead{session})

	if !env.sp.IsRunning(sessionName) {
		t.Fatalf("session %q recycled; claim-holder within its larger threshold must be left running", sessionName)
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", session.ID, err)
	}
	if got.Metadata["continuation_reset_pending"] != "" {
		t.Fatalf("continuation_reset_pending = %q, want empty", got.Metadata["continuation_reset_pending"])
	}
	if strings.Contains(env.stderr.String(), "stalled") {
		t.Fatalf("stderr = %q, want no stall diagnostic", env.stderr.String())
	}
}

// seedInProgressClaim creates an in-progress bead assigned to sessionName, so
// the reconciler's assigned-work check resolves holdsClaim=true. It lets a
// claim-holder-recycler test prove that a protective condition wins over a real
// held claim (rather than over an incidentally claim-less session).
func seedInProgressClaim(t *testing.T, env *restartRequestTestEnv, sessionName string) {
	t.Helper()
	work, err := env.store.Create(beads.Bead{Title: "claimed work", Type: "task", Assignee: sessionName})
	if err != nil {
		t.Fatalf("Create(work): %v", err)
	}
	status := "in_progress"
	if err := env.store.Update(work.ID, beads.UpdateOpts{Status: &status}); err != nil {
		t.Fatalf("Update(work): %v", err)
	}
}

// TestReconcileSessionBeads_ClaimHolderStallFailsSafeOnClaimCheckError is the
// regression guard for the #4137 review blocker: when the assigned-work query
// errors, the reconciler cannot tell whether the session holds a claim, so it
// must recycle via NEITHER recycler. Before the fix the error was encoded as
// holdsClaim=true — inert for the claim-less recycler (which skips holders) but
// the TRIGGER for the claim-holder recycler. The error is store-scoped, so one
// Dolt blip handed the same error to every session in the tick and mass-recycled
// every stale holder in the city, discarding exactly the in-flight work the
// failed check could not rule out. The claim state is unknown, so the holder
// must be left running.
func TestReconcileSessionBeads_ClaimHolderStallFailsSafeOnClaimCheckError(t *testing.T) {
	env, session, sessionName := newProgressStallTestEnv(t)
	env.cfg.Session.ProgressStallTimeout = ""       // claim-less recycler OFF
	env.cfg.Session.ClaimHolderStallTimeout = "20m" // claim-holder recycler ON
	env.store = &assignedWorkListErrorStore{Store: env.store, err: errors.New("assigned work query failed")}

	env.reconcileAtPath(t.TempDir(), []beads.Bead{session})

	if !env.sp.IsRunning(sessionName) {
		t.Fatalf("session %q recycled; an unreadable claim check must not recycle a possible holder", sessionName)
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", session.ID, err)
	}
	if got.Metadata["continuation_reset_pending"] != "" {
		t.Fatalf("continuation_reset_pending = %q, want empty", got.Metadata["continuation_reset_pending"])
	}
	if strings.Contains(env.stderr.String(), "claim-holder-stalled") {
		t.Fatalf("stderr = %q, want no claim-holder-stalled diagnostic", env.stderr.String())
	}
	if !strings.Contains(env.stderr.String(), "checking assigned work before progress-stall recycle") {
		t.Fatalf("stderr = %q, want claim-check-error diagnostic", env.stderr.String())
	}
}

// TestReconcileSessionBeads_ClaimHolderStallDoesNotRecycleProtectedHolder is the
// claim-holder-enabled mirror of ProgressStallDoesNotRecycleExemptOrSafeSessions.
// The claim-less table runs with only ProgressStallTimeout set, so every safety
// case there exercises the path with the claim-holder recycler switched OFF —
// leaving the exact guarantees the new predicate re-opens untested. Each case
// here seeds a real in-progress claim, enables only the claim-holder recycler,
// applies one protective condition, and asserts the holder is left running. For
// the non-exempt cases (provider-red, recent/unknown activity, sub-floor
// threshold) the seeded claim resolves holdsClaim=true, so the recycler would
// fire absent the protection; for the exempt cases (attached, startup lease,
// observation-error) the claim check is short-circuited and the guarantee is the
// exemption itself.
func TestReconcileSessionBeads_ClaimHolderStallDoesNotRecycleProtectedHolder(t *testing.T) {
	tests := []struct {
		name      string
		cityPath  func(t *testing.T) string
		configure func(t *testing.T, env *restartRequestTestEnv, session *beads.Bead, sessionName string)
		provider  func(env *restartRequestTestEnv) runtime.Provider
		wantLog   string
	}{
		{
			name: "attached session",
			configure: func(_ *testing.T, env *restartRequestTestEnv, _ *beads.Bead, sessionName string) {
				env.sp.SetAttached(sessionName, true)
			},
		},
		{
			name: "claim check error fails safe",
			configure: func(_ *testing.T, env *restartRequestTestEnv, _ *beads.Bead, _ string) {
				env.store = &assignedWorkListErrorStore{Store: env.store, err: errors.New("assigned work query failed")}
			},
			wantLog: "checking assigned work before progress-stall recycle",
		},
		{
			name: "attachment check error fails safe",
			configure: func(_ *testing.T, env *restartRequestTestEnv, session *beads.Bead, _ string) {
				env.store = &sessionObservationGetErrorStore{
					Store:     env.store,
					id:        session.ID,
					remaining: 1,
					err:       errors.New("attachment observation failed"),
				}
			},
			wantLog: "checking attachment before progress-stall recycle",
		},
		{
			name: "provider health red",
			cityPath: func(t *testing.T) string {
				dir := t.TempDir()
				writeHealthCache(t, dir, "zai", "unhealthy", nowSecs())
				return dir
			},
		},
		{
			name: "recent provider activity",
			configure: func(_ *testing.T, env *restartRequestTestEnv, _ *beads.Bead, sessionName string) {
				env.sp.SetActivity(sessionName, env.clk.Now().Add(-time.Minute))
			},
		},
		{
			name: "unknown provider activity fails safe",
			provider: func(env *restartRequestTestEnv) runtime.Provider {
				return capabilityOverrideProvider{
					Provider: env.sp,
					caps: runtime.ProviderCapabilities{
						CanReportAttachment: true,
						CanReportActivity:   false,
					},
					sleepCap: runtime.SessionSleepCapabilityTimedOnly,
				}
			},
		},
		{
			name: "startup in-flight lease",
			configure: func(_ *testing.T, env *restartRequestTestEnv, session *beads.Bead, _ string) {
				env.setSessionMetadata(session, map[string]string{
					"pending_create_claim": "true",
					"state":                string(sessionpkg.StateCreating),
					"last_woke_at":         env.clk.Now().UTC().Format(time.RFC3339),
				})
			},
		},
		{
			name: "timeout below enforced minimum",
			configure: func(_ *testing.T, env *restartRequestTestEnv, _ *beads.Bead, sessionName string) {
				env.cfg.Session.ClaimHolderStallTimeout = "30s"
				env.sp.SetActivity(sessionName, env.clk.Now().Add(-time.Minute))
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env, session, sessionName := newProgressStallTestEnv(t)
			env.cfg.Session.ProgressStallTimeout = ""       // isolate the claim-holder recycler
			env.cfg.Session.ClaimHolderStallTimeout = "20m" // claim-holder recycler ON
			seedInProgressClaim(t, env, sessionName)

			cityPath := t.TempDir()
			if tc.cityPath != nil {
				cityPath = tc.cityPath(t)
			}
			if tc.configure != nil {
				tc.configure(t, env, &session, sessionName)
			}
			sp := runtime.Provider(env.sp)
			if tc.provider != nil {
				sp = tc.provider(env)
			}

			env.reconcileAtPathWithProvider(cityPath, sp, []beads.Bead{session})

			if !env.sp.IsRunning(sessionName) {
				t.Fatalf("session %q was recycled; a protected claim-holder must be left running", sessionName)
			}
			got, err := env.store.Get(session.ID)
			if err != nil {
				t.Fatalf("store.Get(%s): %v", session.ID, err)
			}
			if got.Metadata["continuation_reset_pending"] != "" {
				t.Fatalf("continuation_reset_pending = %q, want empty", got.Metadata["continuation_reset_pending"])
			}
			if strings.Contains(env.stderr.String(), "claim-holder-stalled") {
				t.Fatalf("stderr = %q, want no claim-holder-stalled diagnostic", env.stderr.String())
			}
			if tc.wantLog != "" && !strings.Contains(env.stderr.String(), tc.wantLog) {
				t.Fatalf("stderr = %q, want %q", env.stderr.String(), tc.wantLog)
			}
		})
	}
}

// TestReconcileSessionBeads_ProgressStallExemptsMinFloorIdleWorker drives the
// reconciler's pool-counting branch (not just the extracted predicate): a stale,
// claimless, healthy session whose pool is at its configured floor
// (min_active_sessions == open == 1) must be left running. The floor worker is
// waiting for routed work, not parked on an error, so it is exempt from the
// progress-stall recycler.
func TestReconcileSessionBeads_ProgressStallExemptsMinFloorIdleWorker(t *testing.T) {
	env, session, sessionName := newProgressStallTestEnv(t)
	env.cfg.Agents[0].MinActiveSessions = restartRequestTestIntPtr(1)

	// Pool at floor: this single open session is the entire always-warm
	// contingent (open == min == 1).
	env.reconcileAtPath(t.TempDir(), []beads.Bead{session})

	if !env.sp.IsRunning(sessionName) {
		t.Fatalf("session %q was recycled; floor worker at pool floor must be exempt", sessionName)
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", session.ID, err)
	}
	if got.Metadata["restart_requested"] != "" {
		t.Fatalf("restart_requested = %q, want empty for exempt floor worker", got.Metadata["restart_requested"])
	}
	if got.Metadata["continuation_reset_pending"] != "" {
		t.Fatalf("continuation_reset_pending = %q, want empty", got.Metadata["continuation_reset_pending"])
	}
	if strings.Contains(env.stderr.String(), "progress-stalled") {
		t.Fatalf("stderr = %q, want no progress-stalled diagnostic", env.stderr.String())
	}
}

// TestReconcileSessionBeads_ProgressStallRecyclesAboveFloorWorker is the
// counter-case proving the floor exemption is floor-bounded, not blanket: with
// the same min_active_sessions floor of 1 but two open sessions in the pool
// (open == 2 > min == 1), a stale claimless session is above the always-warm
// contingent and IS recycled.
func TestReconcileSessionBeads_ProgressStallRecyclesAboveFloorWorker(t *testing.T) {
	env, session, sessionName := newProgressStallTestEnv(t)
	env.cfg.Agents[0].MinActiveSessions = restartRequestTestIntPtr(1)
	env.cfg.Agents[0].MaxActiveSessions = restartRequestTestIntPtr(2)

	// A second open worker session lifts the pool above its floor (open == 2 >
	// min == 1), so the stale session under test is no longer floor-protected.
	companion := env.createSessionBead("worker-floor-companion")

	env.reconcileAtPath(t.TempDir(), []beads.Bead{session, companion})

	if env.sp.IsRunning(sessionName) {
		t.Fatalf("session %q still running; above-floor stale claimless session should be recycled", sessionName)
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", session.ID, err)
	}
	if got.Metadata["continuation_reset_pending"] != "true" {
		t.Fatalf("continuation_reset_pending = %q, want true", got.Metadata["continuation_reset_pending"])
	}
	if !strings.Contains(env.stderr.String(), "progress-stalled") {
		t.Fatalf("stderr = %q, want progress-stalled diagnostic", env.stderr.String())
	}
}
