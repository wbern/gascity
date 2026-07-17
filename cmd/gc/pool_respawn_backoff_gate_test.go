package main

import (
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// TestPoolRespawnBackoff_ObserveSnapshotArmsGateWithMatchingKey drives a real
// drained "no-wake-reason" pool session bead through the full observation path:
// config parse -> tracker enable -> observeSnapshot classification -> the
// template key it produces. This is the load-bearing link the direct-injection
// gate test skips: observeSnapshot keys drains by normalizedSessionTemplateInfo,
// while the gate keys refusals by cfgAgent.QualifiedName(). If those two ever
// diverge the whole feature silently becomes a no-op with every other test still
// green. This test fails loudly on that divergence, and also proves the
// session.pool_respawn_backoff_base config knob actually enables the mechanism.
func TestPoolRespawnBackoff_ObserveSnapshotArmsGateWithMatchingKey(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Session:   config.SessionConfig{PoolRespawnBackoffBase: "5s", PoolRespawnBackoffMax: "60s"},
		Agents: []config.Agent{{
			Name:              "claude",
			Dir:               "rig",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(3),
		}},
	}
	const template = "rig/claude"

	drained := beads.Bead{
		ID:     "s-drained-1",
		Title:  template + "-1",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel, "template:" + template},
		Metadata: map[string]string{
			"session_name":         "claude-gc-1",
			"template":             template,
			"agent_name":           template + "-1",
			"pool_slot":            "1",
			poolManagedMetadataKey: boolMetadata(true),
			"state":                "drained",
			"sleep_reason":         string(sessionpkg.SleepReasonNoWakeReason),
		},
	}

	tr := newPoolRespawnBackoffTracker(poolRespawnBackoffConfig{})
	now := time.Unix(1_000_000, 0)
	rec := &events.Fake{}
	backedOff := applyPoolRespawnBackoffObservation(tr, cfg, newSessionBeadSnapshot([]beads.Bead{drained}), now, rec)

	if !backedOff[template] {
		t.Fatalf("observeSnapshot did not arm the gate for %q; got %v. The observe-side key "+
			"(normalizedSessionTemplateInfo) must equal the gate-side key (QualifiedName).", template, backedOff)
	}

	// The arm must emit an observability event for correlation with breaker trips.
	var armed *events.Event
	for i := range rec.Events {
		if rec.Events[i].Type == events.PoolRespawnBackoffArmed {
			armed = &rec.Events[i]
			break
		}
	}
	if armed == nil {
		t.Fatalf("no PoolRespawnBackoffArmed event emitted; got %d events", len(rec.Events))
	}
	if armed.Subject != template {
		t.Fatalf("PoolRespawnBackoffArmed subject = %q, want template %q", armed.Subject, template)
	}

	// And the produced key must actually block a create when fed to the gate.
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	cfg.Agents[0].ScaleCheck = "printf 1"
	res := buildDesiredStateWithSessionBeads(
		"test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(),
		store, nil, newSessionBeadSnapshot([]beads.Bead{drained}), nil, io.Discard,
		backedOff,
	)
	for _, tp := range res.State {
		if tp.TemplateName == template {
			t.Fatalf("observed backoff set did not block a fresh create for %q: %+v", template, tp)
		}
	}
}

// TestBuildDesiredState_RespawnBackoffGatesDependencyFloorCreate proves the
// backoff gate is an INVARIANT at the shared create choke point, not just on the
// demand path. Here "db" has zero scale_check demand and is created ONLY because
// a live "helper" root depends on it — the dependency-floor create path
// (selectOrCreateDependencyPoolSessionBead), which bypasses the demand-path gate.
// With "db" under backoff, the floor create must be refused. Without the
// choke-point gate this is the RED case (db floor created despite backoff).
func TestBuildDesiredState_RespawnBackoffGatesDependencyFloorCreate(t *testing.T) {
	newCfg := func() *config.City {
		return &config.City{
			Agents: []config.Agent{
				{Name: "db", StartCommand: "true", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), ScaleCheck: "printf 0"},
				{Name: "helper", Suspended: true, MaxActiveSessions: intPtr(1), DependsOn: []string{"db"}, StartCommand: "echo"},
			},
		}
	}
	// A live "helper" root (state=creating) makes db a realized dependency floor.
	helperBead := beads.Bead{
		Title:  "helper",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:helper"},
		Metadata: map[string]string{
			"template":     "helper",
			"session_name": "s-gc-100",
			"state":        "creating",
		},
	}
	dbSlots := func(res DesiredStateResult) int {
		n := 0
		for _, tp := range res.State {
			if tp.TemplateName == "db" {
				n++
			}
		}
		return n
	}
	build := func(t *testing.T, backedOff map[string]bool) DesiredStateResult {
		cityPath := t.TempDir()
		store := beads.NewMemStore()
		if _, err := store.Create(helperBead); err != nil {
			t.Fatal(err)
		}
		snap, err := loadSessionBeadSnapshot(store)
		if err != nil {
			t.Fatalf("load snapshot: %v", err)
		}
		return buildDesiredStateWithSessionBeads(
			"test-city", cityPath, time.Now().UTC(), newCfg(), runtime.NewFake(),
			store, nil, snap, nil, io.Discard, backedOff,
		)
	}

	t.Run("control creates the dependency floor", func(t *testing.T) {
		if got := dbSlots(build(t, nil)); got != 1 {
			t.Fatalf("control: db dependency-floor slots = %d, want 1", got)
		}
	})
	t.Run("backoff blocks the dependency floor", func(t *testing.T) {
		if got := dbSlots(build(t, map[string]bool{"db": true})); got != 0 {
			t.Fatalf("backoff: db dependency-floor slots = %d, want 0 (choke-point gate must block the floor create)", got)
		}
	})
}

// TestRefreshDesiredState_RespawnBackoffGatesDependencyFloor covers the exact
// path the second review flagged: the post-build overlay refresh
// (refreshDesiredStateWithSessionBeads) builds its OWN agentBuildParams. The
// backoff set must ride through on DesiredStateResult and be re-applied there, or
// the dependency-floor create in the refresh escapes the gate. Control creates
// the floor via refresh; backed-off blocks it.
func TestRefreshDesiredState_RespawnBackoffGatesDependencyFloor(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "db", StartCommand: "true", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), ScaleCheck: "printf 0"},
			{Name: "helper", Suspended: true, MaxActiveSessions: intPtr(1), DependsOn: []string{"db"}, StartCommand: "echo"},
		},
	}
	dbSlots := func(res DesiredStateResult) int {
		n := 0
		for _, tp := range res.State {
			if tp.TemplateName == "db" {
				n++
			}
		}
		return n
	}
	refresh := func(t *testing.T, backedOff map[string]bool) DesiredStateResult {
		cityPath := t.TempDir()
		store := beads.NewMemStore()
		if _, err := store.Create(beads.Bead{
			Title:  "helper",
			Type:   sessionBeadType,
			Labels: []string{sessionBeadLabel, "template:helper"},
			Metadata: map[string]string{
				"template":     "helper",
				"session_name": "s-gc-100",
				"state":        "creating",
			},
		}); err != nil {
			t.Fatal(err)
		}
		snap, err := loadSessionBeadSnapshot(store)
		if err != nil {
			t.Fatalf("load snapshot: %v", err)
		}
		result := DesiredStateResult{
			BaseState:                   map[string]TemplateParams{},
			BeaconTime:                  time.Now().UTC(),
			PoolRespawnBackoffTemplates: backedOff,
		}
		return refreshDesiredStateWithSessionBeads(result, "test-city", cityPath, cfg, runtime.NewFake(), store, snap, io.Discard)
	}

	t.Run("control creates the floor via refresh", func(t *testing.T) {
		if got := dbSlots(refresh(t, nil)); got != 1 {
			t.Fatalf("control: db floor slots via refresh = %d, want 1", got)
		}
	})
	t.Run("backoff blocks the floor via refresh", func(t *testing.T) {
		if got := dbSlots(refresh(t, map[string]bool{"db": true})); got != 0 {
			t.Fatalf("backoff: db floor slots via refresh = %d, want 0 (backoff set must ride the result into the refresh bp)", got)
		}
	})
}

// TestBuildDesiredState_RespawnBackoffBlocksFreshPoolCreate is the end-to-end
// storm repro (upstream gastownhall/gascity#3279). A pool template with live
// scale_check demand normally gets a fresh session created on every build — the
// lockstep respawn that, when the session cannot claim its work and drains, turns
// into a storm. When the reconciler reports the template as under respawn
// backoff, buildDesiredStateWithSessionBeads must refuse the fresh create: no
// desired-state entry AND no session bead written. Without the gate the "backoff"
// subtest creates a session exactly like the control (this is the RED case).
func TestBuildDesiredState_RespawnBackoffBlocksFreshPoolCreate(t *testing.T) {
	newCfg := func() *config.City {
		return &config.City{
			Workspace: config.Workspace{Name: "test-city"},
			Agents: []config.Agent{{
				Name:              "claude",
				Dir:               "rig",
				StartCommand:      "true",
				MaxActiveSessions: intPtr(3),
				ScaleCheck:        "printf 1",
			}},
		}
	}
	const template = "rig/claude"

	// Control: no backoff set -> demand realizes one fresh pool create.
	t.Run("control creates without backoff", func(t *testing.T) {
		cityPath := t.TempDir()
		store := beads.NewMemStore()
		res := buildDesiredStateWithSessionBeads(
			"test-city", cityPath, time.Now().UTC(), newCfg(), runtime.NewFake(),
			store, nil, newSessionBeadSnapshot(nil), nil, io.Discard,
		)
		if len(res.State) != 1 {
			t.Fatalf("control: desired sessions = %d, want 1", len(res.State))
		}
		beadsAfter, err := loadSessionBeads(store)
		if err != nil {
			t.Fatalf("load session beads: %v", err)
		}
		if len(beadsAfter) != 1 {
			t.Fatalf("control: session beads = %d, want 1 fresh create", len(beadsAfter))
		}
	})

	// Gated: the template is under backoff -> no fresh create, no bead written.
	t.Run("backoff blocks fresh create", func(t *testing.T) {
		cityPath := t.TempDir()
		store := beads.NewMemStore()
		res := buildDesiredStateWithSessionBeads(
			"test-city", cityPath, time.Now().UTC(), newCfg(), runtime.NewFake(),
			store, nil, newSessionBeadSnapshot(nil), nil, io.Discard,
			map[string]bool{template: true},
		)
		for _, tp := range res.State {
			if tp.TemplateName == template {
				t.Fatalf("backoff: template %q got a fresh desired session %+v, want none", template, tp)
			}
		}
		beadsAfter, err := loadSessionBeads(store)
		if err != nil {
			t.Fatalf("load session beads: %v", err)
		}
		if len(beadsAfter) != 0 {
			t.Fatalf("backoff: session beads = %d, want 0 (fresh create blocked)", len(beadsAfter))
		}
	})

	// A backoff on a DIFFERENT template must not block this one — the gate is
	// per-template, so unrelated pools keep serving demand.
	t.Run("unrelated backoff does not block", func(t *testing.T) {
		cityPath := t.TempDir()
		store := beads.NewMemStore()
		res := buildDesiredStateWithSessionBeads(
			"test-city", cityPath, time.Now().UTC(), newCfg(), runtime.NewFake(),
			store, nil, newSessionBeadSnapshot(nil), nil, io.Discard,
			map[string]bool{"some/other": true},
		)
		if len(res.State) != 1 {
			t.Fatalf("unrelated backoff: desired sessions = %d, want 1", len(res.State))
		}
	})
}
