package main

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// nudgeInteractivePromptTarget builds a running claude nudge target backed by a
// fake provider whose pane capture is `pane`, with a session that observes as
// running-but-not-busy. Returns the target, the fake, and the created session id.
func nudgeInteractivePromptTarget(t *testing.T, dir string, store beads.Store, fake *runtime.Fake, pane string) (nudgeTarget, string) {
	t.Helper()
	mgr := newSessionManagerWithConfig(dir, store, fake, nil)
	info, err := mgr.Create(context.Background(), "worker", "Worker", "claude", dir, "claude", nil, session.ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fake.SetPeekOutput(info.SessionName, pane)

	prevManaged := nudgeCityUsesManagedReconciler
	prevObserve := nudgeObserveTarget
	nudgeCityUsesManagedReconciler = func(string) bool { return false }
	nudgeObserveTarget = func(nudgeTarget, beads.Store, runtime.Provider) (worker.LiveObservation, error) {
		// Running, LastActivity nil => not busy by activity: the ONLY thing that
		// makes this session unsafe for live delivery is the open prompt.
		return worker.LiveObservation{Running: true}, nil
	}
	t.Cleanup(func() {
		nudgeCityUsesManagedReconciler = prevManaged
		nudgeObserveTarget = prevObserve
	})

	target := nudgeTarget{
		cityPath:    dir,
		cfg:         &config.City{Agents: []config.Agent{{Name: "worker", Provider: "claude"}}},
		sessionID:   info.ID,
		sessionName: info.SessionName,
		identity:    "worker",
		agent:       config.Agent{Name: "worker", Provider: "claude"},
	}
	return target, info.SessionName
}

// TestDeliverSessionNudgeWaitIdleQueuesWhenAtInteractivePrompt is the #2892
// regression guard: a wait-idle nudge to a session parked at an interactive
// option menu must be QUEUED as a deferred reminder, never injected as
// keystrokes (which would submit the focused option).
func TestDeliverSessionNudgeWaitIdleQueuesWhenAtInteractivePrompt(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	store := openNudgeBeadStore(dir)
	fake := runtime.NewFake()

	pane := "Which library should we use?\n❯ 1. date-fns\n  2. luxon\n  3. moment\n"
	target, sessionName := nudgeInteractivePromptTarget(t, dir, store.Store, fake, pane)

	var stdout, stderr bytes.Buffer
	code := deliverSessionNudgeWithWorker(target, store.Store, fake, "OVERRIDE: claim a-003", nudgeDeliveryWaitIdle, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("deliverSessionNudgeWithWorker = %d, stderr=%q, want 0", code, stderr.String())
	}

	pending, inFlight, dead, err := listQueuedNudgesForTarget(dir, target, time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudgesForTarget: %v", err)
	}
	if len(pending) != 1 || len(inFlight) != 0 || len(dead) != 0 {
		t.Fatalf("pending/inFlight/dead = %d/%d/%d, want 1/0/0 (queued, not injected)", len(pending), len(inFlight), len(dead))
	}
	if n := fake.CountCalls("Nudge", sessionName); n != 0 {
		t.Fatalf("live Nudge calls = %d, want 0 (must not inject into the open prompt)", n)
	}
}

// TestWorkerNudgeTargetAtInteractivePromptWiring exercises the peek→matcher
// wiring end to end through the fake provider for both polarities: a menu pane
// is detected, and a bare input prompt / finished-work pane is NOT (no
// false-positive that would divert a nudge away from a genuinely idle session).
func TestWorkerNudgeTargetAtInteractivePromptWiring(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	cases := []struct {
		name string
		pane string
		want bool
	}{
		{"selection menu", "Pick one:\n❯ 1. alpha\n  2. beta\n", true},
		{"bare prompt", "I have finished the task and all tests pass.\n❯ \n", false},
		{"empty pane", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			store := openNudgeBeadStore(dir)
			fake := runtime.NewFake()
			target, _ := nudgeInteractivePromptTarget(t, dir, store.Store, fake, tc.pane)
			if got := workerNudgeTargetAtInteractivePrompt(target, fake); got != tc.want {
				t.Fatalf("workerNudgeTargetAtInteractivePrompt = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestWorkerNudgeTargetAtInteractivePromptFailsOpen verifies the peek helper
// returns false (falls back to existing delivery) when the pane cannot be
// captured, rather than erroring or diverting delivery.
func TestWorkerNudgeTargetAtInteractivePromptFailsOpen(t *testing.T) {
	if workerNudgeTargetAtInteractivePrompt(nudgeTarget{}, nil) {
		t.Fatal("want false for a nil provider (fail open)")
	}
	if workerNudgeTargetAtInteractivePrompt(nudgeTarget{sessionName: "x"}, runtime.NewFake()) {
		t.Fatal("want false when peek returns no output (fail open)")
	}
}
