package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// Every writer of started_config_hash must stamp the partition sub-hashes
// (started_provision_hash/started_launch_hash) from the SAME config, or a later
// launch-only drift check (B2.3) would compare against a stale baseline and
// mis-route a restart. This pins both rebaseline writers to that invariant.
func TestStartedHashWriters_StampPartitionSubHashes(t *testing.T) {
	cfg := runtime.Config{
		Command:      "claude --model opus",
		PreStart:     []string{"echo provisioning"},
		SessionSetup: []string{"echo launching"},
	}
	wantProvision := runtime.ProvisionFingerprint(cfg)
	wantLaunch := runtime.LaunchFingerprint(cfg)
	wantCore := runtime.CoreFingerprint(cfg)

	t.Run("softReloadAcceptedHashMetadata", func(t *testing.T) {
		meta, err := softReloadAcceptedHashMetadata(cfg, wantCore)
		if err != nil {
			t.Fatalf("softReloadAcceptedHashMetadata: %v", err)
		}
		if meta["started_config_hash"] != wantCore {
			t.Errorf("started_config_hash = %q, want %q", meta["started_config_hash"], wantCore)
		}
		if meta["started_provision_hash"] != wantProvision {
			t.Errorf("started_provision_hash = %q, want %q", meta["started_provision_hash"], wantProvision)
		}
		if meta["started_launch_hash"] != wantLaunch {
			t.Errorf("started_launch_hash = %q, want %q", meta["started_launch_hash"], wantLaunch)
		}
	})

	t.Run("sessionHashRebaselineMetadata", func(t *testing.T) {
		meta, err := sessionHashRebaselineMetadata(cfg)
		if err != nil {
			t.Fatalf("sessionHashRebaselineMetadata: %v", err)
		}
		if meta["started_config_hash"] != wantCore {
			t.Errorf("started_config_hash = %q, want %q", meta["started_config_hash"], wantCore)
		}
		if meta["started_provision_hash"] != wantProvision {
			t.Errorf("started_provision_hash = %q, want %q", meta["started_provision_hash"], wantProvision)
		}
		if meta["started_launch_hash"] != wantLaunch {
			t.Errorf("started_launch_hash = %q, want %q", meta["started_launch_hash"], wantLaunch)
		}
	})
}

func TestAcceptConfigDriftAcrossSessions_UpdatesStaleHash(t *testing.T) {
	store := beads.NewMemStore()
	sessionBead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":        "worker",
			"template":            "worker",
			"started_config_hash": "stale-hash-deadbeef",
		},
	})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}

	desired := map[string]TemplateParams{
		"worker": {
			Command:      "new-cmd",
			SessionName:  "worker",
			TemplateName: "worker",
		},
	}
	wantHash := runtime.CoreFingerprint(runtime.Config{Command: "new-cmd"})
	if wantHash == "stale-hash-deadbeef" {
		t.Fatalf("test setup: stale fixture coincidentally equals fresh hash %q", wantHash)
	}

	var stderr bytes.Buffer
	got := acceptConfigDriftAcrossSessions(sessionFrontDoor(store), desired, nil, nil, nil, &stderr)
	if got.Updated != 1 {
		t.Fatalf("updated = %d, want 1 (stderr=%s)", got.Updated, stderr.String())
	}

	updated, err := store.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	if updated.Metadata["started_config_hash"] != wantHash {
		t.Errorf("started_config_hash = %q, want %q", updated.Metadata["started_config_hash"], wantHash)
	}
	var gotBreakdown runtime.BreakdownV1
	if err := json.Unmarshal([]byte(updated.Metadata["core_hash_breakdown"]), &gotBreakdown); err != nil {
		t.Fatalf("core_hash_breakdown is not valid JSON: %v", err)
	}
	wantBreakdown := runtime.CoreFingerprintBreakdown(runtime.Config{Command: "new-cmd"})
	if gotBreakdown.Fields["Command"] != wantBreakdown.Fields["Command"] {
		t.Errorf("core_hash_breakdown[Command] = %q, want %q", gotBreakdown.Fields["Command"], wantBreakdown.Fields["Command"])
	}
}

// TestAcceptConfigDriftAcrossSessions_SkipsUnstartedSessions asserts that a
// session that has never recorded a started_config_hash (still in the
// startup window) is left alone. Stamping a hash on an unstarted session
// would interfere with the reconciler's first-start path; the reconciler
// already skips drift detection while started_config_hash is empty.
func TestAcceptConfigDriftAcrossSessions_SkipsUnstartedSessions(t *testing.T) {
	store := beads.NewMemStore()
	sessionBead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "worker",
			"template":     "worker",
			// no started_config_hash — session hasn't reached started state yet
		},
	})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}

	desired := map[string]TemplateParams{
		"worker": {Command: "new-cmd", SessionName: "worker", TemplateName: "worker"},
	}

	var stderr bytes.Buffer
	got := acceptConfigDriftAcrossSessions(sessionFrontDoor(store), desired, nil, nil, nil, &stderr)
	if got.Updated != 0 {
		t.Fatalf("updated = %d, want 0 for unstarted session (stderr=%s)", got.Updated, stderr.String())
	}

	unchanged, err := store.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	if _, present := unchanged.Metadata["started_config_hash"]; present {
		t.Errorf("started_config_hash unexpectedly written for unstarted session: %q", unchanged.Metadata["started_config_hash"])
	}
}

// TestAcceptConfigDriftAcrossSessions_SkipsOrphanedSessions asserts that a
// session whose name has no entry in the freshly-built desired state
// (orphaned by the config edit — e.g. an agent was removed) is left
// untouched. The orphan/suspended branch of the reconciler handles such
// sessions on the next tick; soft-reload only updates sessions still
// mapped to a configured agent.
func TestAcceptConfigDriftAcrossSessions_SkipsOrphanedSessions(t *testing.T) {
	store := beads.NewMemStore()
	const staleHash = "stale-orphan-hash"
	sessionBead, err := store.Create(beads.Bead{
		Title:  "removed-agent",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":        "removed-agent",
			"template":            "removed-agent",
			"started_config_hash": staleHash,
		},
	})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}

	// Desired state has a different agent — the original session is orphaned.
	desired := map[string]TemplateParams{
		"surviving-agent": {Command: "cmd", SessionName: "surviving-agent", TemplateName: "surviving-agent"},
	}

	var stderr bytes.Buffer
	got := acceptConfigDriftAcrossSessions(sessionFrontDoor(store), desired, nil, nil, nil, &stderr)
	if got.Updated != 0 {
		t.Fatalf("updated = %d, want 0 for orphaned session (stderr=%s)", got.Updated, stderr.String())
	}

	unchanged, err := store.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	if unchanged.Metadata["started_config_hash"] != staleHash {
		t.Errorf("started_config_hash changed for orphan = %q, want %q", unchanged.Metadata["started_config_hash"], staleHash)
	}
}

// TestAcceptConfigDriftAcrossSessions_LeavesNonDriftingSessionsAlone asserts
// that a session whose stored hash already matches the recomputed current
// hash is not rewritten — the function returns 0 and metadata is
// untouched. This keeps soft-reload a no-op for the common no-drift case.
func TestAcceptConfigDriftAcrossSessions_LeavesNonDriftingSessionsAlone(t *testing.T) {
	store := beads.NewMemStore()
	matchingHash := runtime.CoreFingerprint(runtime.Config{Command: "cmd"})
	sessionBead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":        "worker",
			"template":            "worker",
			"started_config_hash": matchingHash,
		},
	})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}

	desired := map[string]TemplateParams{
		"worker": {Command: "cmd", SessionName: "worker", TemplateName: "worker"},
	}

	var stderr bytes.Buffer
	got := acceptConfigDriftAcrossSessions(sessionFrontDoor(store), desired, nil, nil, nil, &stderr)
	if got.Updated != 0 {
		t.Fatalf("updated = %d, want 0 (no drift) — stderr=%s", got.Updated, stderr.String())
	}

	unchanged, err := store.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	if unchanged.Metadata["started_config_hash"] != matchingHash {
		t.Errorf("started_config_hash rewritten unexpectedly = %q, want %q", unchanged.Metadata["started_config_hash"], matchingHash)
	}
}

func TestAcceptConfigDriftAcrossSessions_CancelsExistingConfigDriftDrain(t *testing.T) {
	store := beads.NewMemStore()
	oldHash := runtime.CoreFingerprint(runtime.Config{Command: "old-cmd"})
	sessionBead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":        "worker",
			"template":            "worker",
			"started_config_hash": oldHash,
			"generation":          "7",
		},
	})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}

	sp := runtime.NewFake()
	dt := newDrainTracker()
	clk := &clock.Fake{Time: time.Unix(100, 0)}
	if !beginSessionDrainInfo(sessiontest.SeedBead(t, sessionBead), sp, dt, "config-drift", clk, time.Minute) {
		t.Fatal("beginSessionDrainInfo returned false")
	}

	desired := map[string]TemplateParams{
		"worker": {Command: "new-cmd", SessionName: "worker", TemplateName: "worker"},
	}
	var stderr bytes.Buffer
	got := acceptConfigDriftAcrossSessions(sessionFrontDoor(store), desired, nil, sp, dt, &stderr)
	if got.Updated != 1 {
		t.Fatalf("updated = %d, want 1 (stderr=%s)", got.Updated, stderr.String())
	}
	if got.CanceledDrains != 1 {
		t.Fatalf("canceled drains = %d, want 1", got.CanceledDrains)
	}
	if drain := dt.get(sessionBead.ID); drain != nil {
		t.Fatalf("config-drift drain still queued after soft acceptance: %+v", drain)
	}
}

func TestAcceptConfigDriftAcrossSessions_FailsAckedDrainWithoutProviderBeforeMetadataWrite(t *testing.T) {
	store := beads.NewMemStore()
	oldHash := runtime.CoreFingerprint(runtime.Config{Command: "old-cmd"})
	sessionBead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":        "worker",
			"template":            "worker",
			"started_config_hash": oldHash,
			"generation":          "7",
		},
	})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}

	dt := newDrainTracker()
	dt.set(sessionBead.ID, &drainState{
		reason:     "config-drift",
		generation: 7,
		ackSet:     true,
	})
	desired := map[string]TemplateParams{
		"worker": {Command: "new-cmd", SessionName: "worker", TemplateName: "worker"},
	}

	var stderr bytes.Buffer
	got := acceptConfigDriftAcrossSessions(sessionFrontDoor(store), desired, nil, nil, dt, &stderr)
	if got.Updated != 0 || got.Failed != 1 {
		t.Fatalf("result = %+v, want updated=0 failed=1 (stderr=%s)", got, stderr.String())
	}
	if drain := dt.get(sessionBead.ID); drain == nil {
		t.Fatal("config-drift drain was removed even though provider cancellation was unavailable")
	}
	unchanged, err := store.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	if unchanged.Metadata["started_config_hash"] != oldHash {
		t.Fatalf("started_config_hash = %q, want unchanged %q", unchanged.Metadata["started_config_hash"], oldHash)
	}
	warnings := strings.Join(got.warnings(), "\n")
	if !strings.Contains(warnings, "worker") {
		t.Fatalf("warnings = %q, want failed session name", warnings)
	}
}

func TestAcceptConfigDriftAcrossSessions_FailsWhenAckMetadataClearFails(t *testing.T) {
	store := beads.NewMemStore()
	oldHash := runtime.CoreFingerprint(runtime.Config{Command: "old-cmd"})
	sessionBead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":        "worker",
			"template":            "worker",
			"started_config_hash": oldHash,
			"generation":          "7",
		},
	})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}

	sp := &removeMetaErrorProvider{Fake: runtime.NewFake(), err: errors.New("injected remove metadata failure")}
	if err := sp.Start(context.Background(), "worker", runtime.Config{Command: "old-cmd"}); err != nil {
		t.Fatalf("Start(worker): %v", err)
	}
	if err := setReconcilerDrainAckMetadata(sp.Fake, "worker", &drainState{reason: "config-drift", generation: 7}); err != nil {
		t.Fatalf("set drain ack metadata: %v", err)
	}
	dt := newDrainTracker()
	dt.set(sessionBead.ID, &drainState{
		reason:     "config-drift",
		generation: 7,
		ackSet:     true,
	})
	desired := map[string]TemplateParams{
		"worker": {Command: "new-cmd", SessionName: "worker", TemplateName: "worker"},
	}

	var stderr bytes.Buffer
	got := acceptConfigDriftAcrossSessions(sessionFrontDoor(store), desired, nil, sp, dt, &stderr)
	if got.Updated != 0 || got.Failed != 1 || got.CanceledDrains != 0 {
		t.Fatalf("result = %+v, want updated=0 failed=1 canceled=0 (stderr=%s)", got, stderr.String())
	}
	if drain := dt.get(sessionBead.ID); drain == nil || !drain.ackSet {
		t.Fatalf("config-drift drain = %+v, want acked drain retained", drain)
	}
	unchanged, err := store.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	if unchanged.Metadata["started_config_hash"] != oldHash {
		t.Fatalf("started_config_hash = %q, want unchanged %q", unchanged.Metadata["started_config_hash"], oldHash)
	}
	if gotAck, err := sp.GetMeta("worker", "GC_DRAIN_ACK"); err != nil || gotAck != "1" {
		t.Fatalf("GC_DRAIN_ACK = %q, %v; want still set after injected remove failure", gotAck, err)
	}
	warnings := strings.Join(got.warnings(), "\n")
	if !strings.Contains(warnings, "worker") {
		t.Fatalf("warnings = %q, want failed session name", warnings)
	}
}

func TestAcceptConfigDriftAcrossSessions_AppliesTemplateOverridesToHash(t *testing.T) {
	store := beads.NewMemStore()
	oldHash := runtime.CoreFingerprint(runtime.Config{Command: "agent --effort low"})
	sessionBead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":        "worker",
			"template":            "worker",
			"started_config_hash": oldHash,
			"template_overrides":  `{"effort":"high"}`,
		},
	})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}

	provider := &config.ResolvedProvider{
		OptionsSchema: []config.ProviderOption{{
			Key:  "effort",
			Type: "select",
			Choices: []config.OptionChoice{
				{Value: "low", FlagArgs: []string{"--effort", "low"}},
				{Value: "high", FlagArgs: []string{"--effort", "high"}},
			},
		}},
		EffectiveDefaults: map[string]string{"effort": "low"},
	}
	desired := map[string]TemplateParams{
		"worker": {
			Command:          "agent --effort low",
			SessionName:      "worker",
			TemplateName:     "worker",
			ResolvedProvider: provider,
		},
	}

	var stderr bytes.Buffer
	got := acceptConfigDriftAcrossSessions(sessionFrontDoor(store), desired, nil, nil, nil, &stderr)
	if got.Updated != 1 {
		t.Fatalf("updated = %d, want 1 (stderr=%s)", got.Updated, stderr.String())
	}
	updated, err := store.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	wantHash := runtime.CoreFingerprint(runtime.Config{Command: "agent --effort high"})
	if updated.Metadata["started_config_hash"] != wantHash {
		t.Fatalf("started_config_hash = %q, want override hash %q", updated.Metadata["started_config_hash"], wantHash)
	}
}

func TestAcceptConfigDriftAcrossSessions_MetadataFailureReportsAndContinues(t *testing.T) {
	base := beads.NewMemStore()
	failing, err := base.Create(beads.Bead{
		Title:  "failing",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":        "failing",
			"template":            "failing",
			"started_config_hash": "stale-failing",
		},
	})
	if err != nil {
		t.Fatalf("Create(failing): %v", err)
	}
	succeeding, err := base.Create(beads.Bead{
		Title:  "succeeding",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":        "succeeding",
			"template":            "succeeding",
			"started_config_hash": "stale-succeeding",
		},
	})
	if err != nil {
		t.Fatalf("Create(succeeding): %v", err)
	}

	store := failingSetMetadataBatchStore{Store: base, failID: failing.ID}
	desired := map[string]TemplateParams{
		"failing":    {Command: "new-failing", SessionName: "failing", TemplateName: "failing"},
		"succeeding": {Command: "new-succeeding", SessionName: "succeeding", TemplateName: "succeeding"},
	}

	var stderr bytes.Buffer
	got := acceptConfigDriftAcrossSessions(sessionFrontDoor(store), desired, nil, nil, nil, &stderr)
	if got.Updated != 1 || got.Failed != 1 {
		t.Fatalf("result = %+v, want updated=1 failed=1 (stderr=%s)", got, stderr.String())
	}
	if !strings.Contains(stderr.String(), "updating config hash metadata for failing") {
		t.Fatalf("stderr = %q, want failing update error", stderr.String())
	}
	warnings := strings.Join(got.warnings(), "\n")
	if !strings.Contains(warnings, "failing") {
		t.Fatalf("warnings = %q, want failing session name", warnings)
	}
	updated, err := base.Get(succeeding.ID)
	if err != nil {
		t.Fatalf("Get(succeeding): %v", err)
	}
	wantHash := runtime.CoreFingerprint(runtime.Config{Command: "new-succeeding"})
	if updated.Metadata["started_config_hash"] != wantHash {
		t.Fatalf("succeeding started_config_hash = %q, want %q", updated.Metadata["started_config_hash"], wantHash)
	}
}

func TestAcceptConfigDriftAcrossSessions_EmptyDesiredReportsOpenSessions(t *testing.T) {
	store := beads.NewMemStore()
	_, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":        "worker",
			"template":            "worker",
			"started_config_hash": "stale-hash",
		},
	})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}

	var stderr bytes.Buffer
	got := acceptConfigDriftAcrossSessions(sessionFrontDoor(store), map[string]TemplateParams{}, nil, nil, nil, &stderr)
	if !got.DesiredEmpty || got.OpenSessions != 1 {
		t.Fatalf("result = %+v, want DesiredEmpty with one open session", got)
	}
}

type failingSetMetadataBatchStore struct {
	beads.Store
	failID string
}

func (s failingSetMetadataBatchStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if id == s.failID {
		return errors.New("injected metadata failure")
	}
	return s.Store.SetMetadataBatch(id, kvs)
}

type s1SoftReloadCASFailureStore struct {
	*beads.MemStore
	fail        bool
	onHashWrite func()
}

func (s *s1SoftReloadCASFailureStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if kvs["started_config_hash"] != "" && s.onHashWrite != nil {
		callback := s.onHashWrite
		s.onHashWrite = nil
		callback()
	}
	return s.MemStore.SetMetadataBatch(id, kvs)
}

func (s *s1SoftReloadCASFailureStore) UpdateIfMatch(id string, revision int64, opts beads.UpdateOpts) error {
	if opts.Metadata["started_config_hash"] != "" && s.onHashWrite != nil {
		callback := s.onHashWrite
		s.onHashWrite = nil
		callback()
	}
	if s.fail {
		return errors.New("injected conditional cancellation write failure")
	}
	return s.MemStore.UpdateIfMatch(id, revision, opts)
}

func TestS1SoftReloadCancelsAdmissionBeforeAcceptingHash(t *testing.T) {
	for _, scenario := range []string{"cancel", "accepted-stop", "corrupt-evidence", "cas-failure"} {
		t.Run(scenario, func(t *testing.T) {
			store := &s1SoftReloadCASFailureStore{MemStore: beads.NewMemStore()}
			if err := beads.StampOpenedStore(store, "memory", gate.Require, nil, nil); err != nil {
				t.Fatal(err)
			}
			oldHash := runtime.CoreFingerprint(runtime.Config{Command: "old-cmd"})
			row, err := store.Create(beads.Bead{Title: "worker", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: map[string]string{"session_name": "worker", "template": "worker", "state": "active", "started_config_hash": oldHash, "generation": "7", "instance_token": "runtime-one"}})
			if err != nil {
				t.Fatal(err)
			}
			front := sessionFrontDoor(store)
			fence := session.ExecutionFence{InstanceToken: "runtime-one", Generation: "7"}
			attempt, err := front.BeginDrainAdmission(row.ID, fence)
			if err != nil {
				t.Fatal(err)
			}
			if err := front.FinishDrainAdmission(row.ID, fence, attempt, true); err != nil {
				t.Fatal(err)
			}
			if scenario == "accepted-stop" {
				if err := front.AcceptDrainStop(row.ID, fence, attempt); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "corrupt-evidence" {
				if err := store.SetMetadata(row.ID, session.WorkStartedKey, "{ corrupt"); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "cas-failure" {
				store.fail = true
			}
			sp := runtime.NewFake()
			if err := sp.Start(context.Background(), "worker", runtime.Config{Command: "old-cmd"}); err != nil {
				t.Fatal(err)
			}
			for k, v := range map[string]string{"GC_DRAIN_ACK": "1", reconcilerDrainAckSourceKey: drainAckSourceAgentValue, drainAdmissionAttemptKey: attempt, drainAckRequesterInstanceTokenKey: drainAckInstanceTokenDigest("runtime-one")} {
				if err := sp.SetMeta("worker", k, v); err != nil {
					t.Fatal(err)
				}
			}
			dt := newDrainTracker()
			dt.set(row.ID, &drainState{reason: "config-drift", generation: 7, ackSet: true, admissionAttempt: attempt})
			desired := map[string]TemplateParams{"worker": {Command: "new-cmd", SessionName: "worker", TemplateName: "worker"}}
			var stderr bytes.Buffer
			result := acceptConfigDriftAcrossSessions(front, desired, nil, sp, dt, &stderr)
			saved, err := store.Get(row.ID)
			if err != nil {
				t.Fatal(err)
			}
			ack, err := sp.GetMeta("worker", "GC_DRAIN_ACK")
			if err != nil {
				t.Fatal(err)
			}
			if scenario != "cancel" {
				if result.Updated != 0 || result.Failed != 1 || saved.Metadata["started_config_hash"] != oldHash || ack != "1" || dt.get(row.ID) == nil {
					t.Fatalf("failed cancellation accepted reload: result=%+v hash=%q ack=%q stderr=%s", result, saved.Metadata["started_config_hash"], ack, stderr.String())
				}
				return
			}
			if result.Updated != 1 || result.CanceledDrains != 1 || dt.get(row.ID) != nil || ack != "" {
				t.Fatalf("reload cancellation failed: result=%+v ack=%q stderr=%s", result, ack, stderr.String())
			}
			if err := front.RecordStartedClaim(row.ID, fence, session.WorkStartedEntry{Bead: "fresh-work"}); err != nil {
				t.Fatalf("soft reload left durable admission closed: %v", err)
			}
			for k, v := range map[string]string{"GC_DRAIN_ACK": "1", reconcilerDrainAckSourceKey: drainAckSourceAgentValue, drainAdmissionAttemptKey: attempt, drainAckRequesterInstanceTokenKey: drainAckInstanceTokenDigest("runtime-one")} {
				if err := sp.SetMeta("worker", k, v); err != nil {
					t.Fatal(err)
				}
			}
			info, _, _, err := front.ExecutionEvidence(row.ID)
			if err != nil {
				t.Fatal(err)
			}
			if qualifiedDrainAck(info, front, sp, "worker", true) {
				t.Fatal("soft-reload canceled attempt authorized late stop")
			}
		})
	}
}

func TestS1SoftReloadRecoversExactConfigDriftAttemptWithoutTracker(t *testing.T) {
	for _, scenario := range []string{"cancel", "accepted", "corrupt", "unrelated", "stale-attempt", "cas-failure", "provider-failure", "wrong-generation", "wrong-session", "wrong-incarnation", "wrong-source", "missing-attempt", "clear-failure", "malformed-frame", "unknown-version", "missing-frame", "wrong-runtime-name", "replacement-after-capture"} {
		for _, nilTracker := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/nil-tracker-%v", scenario, nilTracker), func(t *testing.T) {
				store := &s1SoftReloadCASFailureStore{MemStore: beads.NewMemStore()}
				if err := beads.StampOpenedStore(store, "memory", gate.Require, nil, nil); err != nil {
					t.Fatal(err)
				}
				oldHash := runtime.CoreFingerprint(runtime.Config{Command: "old-cmd"})
				row, err := store.Create(beads.Bead{Title: "worker", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: map[string]string{"session_name": "worker", "template": "worker", "agent_name": "worker", "state": "active", "started_config_hash": oldHash, "generation": "7", "instance_token": "runtime-one"}})
				if err != nil {
					t.Fatal(err)
				}
				front := sessionFrontDoor(store)
				fence := session.ExecutionFence{InstanceToken: "runtime-one", Generation: "7"}
				attempt, err := front.BeginControllerDrainAdmission(row.ID, fence)
				if err != nil {
					t.Fatal(err)
				}
				if scenario == "accepted" {
					if err := front.AcceptControllerDrainStop(row.ID, fence, attempt); err != nil {
						t.Fatal(err)
					}
				}
				if scenario == "stale-attempt" {
					if _, err := front.BeginControllerDrainAdmission(row.ID, fence); err != nil {
						t.Fatal(err)
					}
				}
				if scenario == "corrupt" {
					if err := store.SetMetadata(row.ID, session.WorkStartedKey, "{ corrupt"); err != nil {
						t.Fatal(err)
					}
				}
				before, err := store.Get(row.ID)
				if err != nil {
					t.Fatal(err)
				}
				if scenario == "cas-failure" {
					store.fail = true
				}
				sp := runtime.NewFake()
				if err := sp.Start(context.Background(), "worker", runtime.Config{Command: "old-cmd"}); err != nil {
					t.Fatal(err)
				}
				reason := "config-drift"
				if scenario == "unrelated" {
					reason = "orphaned"
				}
				fields := map[string]string{"GC_DRAIN_ACK": "1", reconcilerDrainAckSourceKey: reconcilerDrainAckSourceValue, reconcilerDrainAckReasonKey: reason, reconcilerDrainAckGenerationKey: "7", drainAdmissionAttemptKey: attempt, "GC_INSTANCE_TOKEN": "runtime-one", "GC_SESSION_ID": row.ID}
				switch scenario {
				case "wrong-generation":
					fields[reconcilerDrainAckGenerationKey] = "6"
				case "wrong-session":
					fields["GC_SESSION_ID"] = "other-session"
				case "wrong-incarnation":
					fields["GC_INSTANCE_TOKEN"] = "old-runtime"
				case "wrong-source":
					fields[reconcilerDrainAckSourceKey] = drainAckSourceAgentValue
				case "missing-attempt":
					fields[drainAdmissionAttemptKey] = ""
				}
				for k, v := range fields {
					if err := sp.SetMeta("worker", k, v); err != nil {
						t.Fatal(err)
					}
				}
				if err := publishDrainAckFrame(sp, "worker", session.Info{ID: fields["GC_SESSION_ID"], SessionNameMetadata: "worker", Generation: fields[reconcilerDrainAckGenerationKey], InstanceToken: fields["GC_INSTANCE_TOKEN"]}, fields[reconcilerDrainAckSourceKey], reason, fields[drainAdmissionAttemptKey]); err != nil {
					t.Fatal(err)
				}
				switch scenario {
				case "malformed-frame":
					if err := sp.SetMeta("worker", drainAckFrameKey, "{ corrupt"); err != nil {
						t.Fatal(err)
					}
				case "missing-frame":
					if err := sp.RemoveMeta("worker", drainAckFrameKey); err != nil {
						t.Fatal(err)
					}
				case "unknown-version", "wrong-runtime-name":
					raw, err := sp.GetMeta("worker", drainAckFrameKey)
					if err != nil {
						t.Fatal(err)
					}
					var frame drainAckFrame
					if err := json.Unmarshal([]byte(raw), &frame); err != nil {
						t.Fatal(err)
					}
					if scenario == "unknown-version" {
						frame.V = 2
					} else {
						frame.RuntimeName = "replacement-name"
					}
					encoded, err := json.Marshal(frame)
					if err != nil {
						t.Fatal(err)
					}
					if err := sp.SetMeta("worker", drainAckFrameKey, string(encoded)); err != nil {
						t.Fatal(err)
					}
				}
				if scenario == "provider-failure" {
					sp.GetMetaErrors["worker"] = map[string]error{drainAckFrameKey: errors.New("provider recovery read unavailable")}
				}
				if scenario == "clear-failure" {
					sp.RemoveMetaErrors["worker"] = map[string]error{"GC_DRAIN_ACK": errors.New("provider ACK clear unavailable")}
				}
				var dt *drainTracker
				if !nilTracker {
					dt = newDrainTracker()
				}
				desired := map[string]TemplateParams{"worker": {Command: "new-cmd", SessionName: "worker", TemplateName: "worker"}}
				var stderr bytes.Buffer
				replacedAfterCapture := false
				var provider runtime.Provider = sp
				if scenario == "replacement-after-capture" {
					provider = &s1FrameReadInterleaveProvider{Provider: sp, once: func() {
						replacedAfterCapture = true
						if err := front.CancelDrainAdmission(row.ID, fence, attempt); err != nil {
							t.Fatal(err)
						}
						if _, err := front.BeginControllerDrainAdmission(row.ID, fence); err != nil {
							t.Fatal(err)
						}
						var err error
						before, err = store.Get(row.ID)
						if err != nil {
							t.Fatal(err)
						}
					}}
				}
				result := acceptConfigDriftAcrossSessions(front, desired, nil, provider, dt, &stderr)
				if scenario == "replacement-after-capture" && !replacedAfterCapture {
					t.Fatal("replacement interleaving did not execute")
				}
				saved, err := store.Get(row.ID)
				if err != nil {
					t.Fatal(err)
				}
				ack, err := sp.GetMeta("worker", "GC_DRAIN_ACK")
				if err != nil {
					t.Fatal(err)
				}
				if scenario == "clear-failure" {
					if result.Updated != 0 || result.Failed != 1 || saved.Metadata["started_config_hash"] != oldHash || ack != "1" {
						t.Fatalf("uncertain provider cleanup accepted hash: result=%+v ack=%q", result, ack)
					}
					_, cell, _, err := front.ExecutionEvidence(row.ID)
					if err != nil || cell.Drain != nil {
						t.Fatalf("clear failure lost durable cancellation: gate=%+v err=%v", cell.Drain, err)
					}
					delete(sp.RemoveMetaErrors, "worker")
					// The stale ACK must remain unable to stop work after cancellation,
					// even though provider cleanup failed after the durable CAS.
					if err := front.RecordStartedClaim(row.ID, fence, session.WorkStartedEntry{Bead: "resumed-after-clear-failure"}); err != nil {
						t.Fatal(err)
					}
					if qualifiedDrainAck(frontInfoForSoftReloadTest(t, front, row.ID), front, sp, "worker", true) {
						t.Fatal("partially cleared canceled ACK authorized stop")
					}
					return
				}
				if scenario != "cancel" && scenario != "unrelated" {
					if result.Updated != 0 || result.Failed != 1 || saved.Metadata["started_config_hash"] != oldHash || saved.Metadata[session.WorkStartedKey] != before.Metadata[session.WorkStartedKey] || ack != "1" {
						t.Fatalf("unproved recovered cancellation accepted hash/changed evidence: result=%+v ack=%q stderr=%s", result, ack, stderr.String())
					}
					return
				}
				if scenario == "unrelated" {
					if result.Updated != 1 || result.CanceledDrains != 0 || saved.Metadata[session.WorkStartedKey] != before.Metadata[session.WorkStartedKey] || ack != "1" {
						t.Fatalf("softreload touched unrelated drain: result=%+v ack=%q", result, ack)
					}
					return
				}
				if result.Updated != 1 || result.CanceledDrains != 1 || ack != "" {
					t.Fatalf("recovered config-drift gate not canceled: result=%+v ack=%q stderr=%s", result, ack, stderr.String())
				}
				work, err := store.Create(beads.Bead{Title: "actual resumed work", Type: "task", Status: "in_progress", Assignee: row.ID})
				if err != nil {
					t.Fatal(err)
				}
				if err := front.RecordStartedClaim(row.ID, fence, session.WorkStartedEntry{Store: "work", Bead: work.ID}); err != nil {
					t.Fatalf("recovered hash acceptance left admission closed: %v", err)
				}
				for k, v := range fields {
					if err := sp.SetMeta("worker", k, v); err != nil {
						t.Fatal(err)
					}
				}
				env := newReconcilerTestEnv()
				env.store = store
				env.sp = sp
				env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}, Beads: config.BeadsConfig{ConditionalWrites: "require"}}
				env.desiredState = desired
				seat, err := store.Get(row.ID)
				if err != nil {
					t.Fatal(err)
				}
				reconcileSessionBeads(context.Background(), []beads.Bead{seat}, desired, configuredSessionNames(env.cfg, "", store), env.cfg, sp, store, newDrainOps(sp), []beads.Bead{work}, nil, env.dt, map[string]int{"worker": 1}, false, map[string]bool{"worker": true, row.ID: true}, "", nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr, env.startOptions...)
				if !sp.IsRunning("worker") || env.sessionInfo(row.ID).StateReason == session.DrainAckStopPendingReason {
					t.Fatal("late recovered config-drift ACK stopped resumed work")
				}
			})
		}
	}
}

func TestS1SoftReloadHashWriteRejectsReplacementDrain(t *testing.T) {
	store := &s1SoftReloadCASFailureStore{MemStore: beads.NewMemStore()}
	if err := beads.StampOpenedStore(store, "memory", gate.Require, nil, nil); err != nil {
		t.Fatal(err)
	}
	oldHash := runtime.CoreFingerprint(runtime.Config{Command: "old-cmd"})
	row, err := store.Create(beads.Bead{Title: "worker", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: map[string]string{"session_name": "worker", "template": "worker", "state": "active", "started_config_hash": oldHash, "generation": "7", "instance_token": "runtime-one"}})
	if err != nil {
		t.Fatal(err)
	}
	front := sessionFrontDoor(store)
	fence := session.ExecutionFence{InstanceToken: "runtime-one", Generation: "7"}
	attempt, err := front.BeginControllerDrainAdmission(row.ID, fence)
	if err != nil {
		t.Fatal(err)
	}
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker", runtime.Config{Command: "old-cmd"}); err != nil {
		t.Fatal(err)
	}
	dt := newDrainTracker()
	dt.set(row.ID, &drainState{reason: "config-drift", generation: 7, admissionAttempt: attempt})
	replacement := ""
	store.onHashWrite = func() {
		var err error
		replacement, err = front.BeginControllerDrainAdmission(row.ID, fence)
		if err != nil {
			t.Fatal(err)
		}
	}
	var stderr bytes.Buffer
	result := acceptConfigDriftAcrossSessions(front, map[string]TemplateParams{"worker": {Command: "new-cmd", SessionName: "worker", TemplateName: "worker"}}, nil, sp, dt, &stderr)
	saved, err := store.Get(row.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, cell, _, err := front.ExecutionEvidence(row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if replacement == "" {
		t.Fatal("hash-write interleaving did not execute")
	}
	if result.Updated != 0 || result.Failed != 1 || result.CanceledDrains != 1 || saved.Metadata["started_config_hash"] != oldHash || cell.Drain == nil || cell.Drain.Attempt != replacement {
		t.Fatalf("hash accepted across replacement drain: result=%+v gate=%+v stderr=%s", result, cell.Drain, stderr.String())
	}
}

func frontInfoForSoftReloadTest(t *testing.T, front *session.Store, id string) session.Info {
	t.Helper()
	info, _, _, err := front.ExecutionEvidence(id)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// s1FrameReadInterleaveProvider replaces native state after the initial capture
// but before recovered provider evidence can be used for cancellation.
type s1FrameReadInterleaveProvider struct {
	runtime.Provider
	once func()
}

func (p *s1FrameReadInterleaveProvider) GetMeta(name, key string) (string, error) {
	if key == drainAckFrameKey && p.once != nil {
		callback := p.once
		p.once = nil
		callback()
	}
	return p.Provider.GetMeta(name, key)
}
