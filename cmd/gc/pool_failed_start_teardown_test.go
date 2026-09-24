package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

func TestPoolFailedStartStopsBeadScopedRuntimeBeforeClosingRow(t *testing.T) {
	store := beads.NewMemStore()
	provider := runtime.NewFake()
	now := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	const template = "example/worker"
	info, err := createPoolSessionBeadWithAlias(store, template, nil, newSessionBeadSnapshot(nil), now, poolSessionCreateIdentity{AgentName: "example/worker-1", Slot: 1}, "")
	if err != nil {
		t.Fatalf("create pool row: %v", err)
	}
	name := info.SessionNameMetadata
	if name != PoolSessionName(template, info.ID) {
		t.Fatalf("runtime name = %q, want bead-scoped name", name)
	}
	if err := provider.Start(context.Background(), name, runtime.Config{}); err != nil {
		t.Fatalf("start simulated runtime: %v", err)
	}
	result := startResult{
		prepared: preparedStart{candidate: startCandidate{info: info, tp: TemplateParams{SessionName: name, TemplateName: template, Command: "true"}}},
		err:      errors.New("start failed after provisioning"), outcome: TraceOutcomeProviderError,
		started: now, finished: now, rollbackPending: true, provider: provider,
	}
	commitStartFailure(result, sessionFrontDoor(store), &clock.Fake{Time: now}, events.Discard, 0, io.Discard, nil)
	row, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("read failed row: %v", err)
	}
	if row.Status != "closed" {
		t.Fatalf("failed row status = %q, want closed after successful teardown", row.Status)
	}
	running, err := provider.ListRunning("")
	if err != nil {
		t.Fatalf("list runtimes: %v", err)
	}
	if len(running) != 0 {
		t.Fatalf("failed row closed with %d live runtime(s): %v", len(running), running)
	}
}

func TestReconcilePoolUnconfirmedCloseWaitsForRuntimeTeardown(t *testing.T) {
	for _, state := range []sessionpkg.State{sessionpkg.StateCreating, sessionpkg.StateFailedCreate} {
		t.Run(string(state), func(t *testing.T) {
			store := beads.NewMemStore()
			provider := runtime.NewFake()
			clk := &clock.Fake{Time: time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)}
			cfg := &config.City{Agents: []config.Agent{{Name: "worker"}}}
			row, err := store.Create(beads.Bead{
				Title: "worker", Type: sessionBeadType, Labels: []string{sessionBeadLabel, "agent:worker"},
				Metadata: map[string]string{
					"session_name": "pending", "agent_name": "worker", "template": "worker", "state": string(state),
					"pool_slot": "1", "pending_create_claim": boolMetadata(true),
					"pending_create_started_at": pendingCreateStartedAtNow(clk.Now().Add(-pendingCreateNeverStartedTimeout - time.Minute)),
					poolManagedMetadataKey:      boolMetadata(true), "generation": "1", "instance_token": "held-token",
				},
			})
			if err != nil {
				t.Fatalf("create row: %v", err)
			}
			name := PoolSessionName("worker", row.ID)
			if err := store.SetMetadata(row.ID, "session_name", name); err != nil {
				t.Fatalf("set runtime name: %v", err)
			}
			provider.StopErrors = map[string]error{name: errors.New("provider unavailable")}
			runTick := func() (beads.Bead, string) {
				t.Helper()
				current, err := store.Get(row.ID)
				if err != nil {
					t.Fatalf("get current row: %v", err)
				}
				var stdout, stderr bytes.Buffer
				reconcileSessionBeads(context.Background(), []beads.Bead{current}, map[string]TemplateParams{},
					configuredSessionNames(cfg, "", store), cfg, provider, store, nil, nil, nil,
					newDrainTracker(), map[string]int{"worker": 1}, false, nil, "", nil, clk,
					events.Discard, 0, 0, &stdout, &stderr)
				got, err := store.Get(row.ID)
				if err != nil {
					t.Fatalf("get reconciled row: %v", err)
				}
				return got, stderr.String()
			}
			for tick := 1; tick <= 3; tick++ {
				got, stderr := runTick()
				if got.Status == "closed" || got.Metadata["session_name"] != name || got.Metadata["state"] != string(state) {
					t.Fatalf("tick %d: unconfirmed row changed while stop failed: status=%q name=%q state=%q stderr=%s", tick, got.Status, got.Metadata["session_name"], got.Metadata["state"], stderr)
				}
			}
			delete(provider.StopErrors, name)
			got, stderr := runTick()
			if got.Status != "closed" {
				t.Fatalf("recovered teardown left row %s open; stderr=%s", got.ID, stderr)
			}
		})
	}
}

func TestCanceledPoolStartHoldsRowWhenRuntimeTeardownFails(t *testing.T) {
	store := beads.NewMemStore()
	provider := runtime.NewFake()
	now := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	const template = "example/worker"
	info, err := createPoolSessionBeadWithAlias(store, template, nil, nil, now,
		poolSessionCreateIdentity{AgentName: "example/worker-1", Slot: 1}, "")
	if err != nil {
		t.Fatalf("create pool row: %v", err)
	}
	name := info.SessionNameMetadata
	if err := provider.Start(context.Background(), name, runtime.Config{}); err != nil {
		t.Fatalf("start simulated runtime: %v", err)
	}
	provider.StopErrors = map[string]error{name: errors.New("provider unavailable")}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := startResult{
		prepared: preparedStart{candidate: startCandidate{info: info, tp: TemplateParams{SessionName: name, TemplateName: template, Command: "true"}}},
		outcome:  TraceOutcomeSessionExistsConverged, started: now, finished: now, provider: provider,
	}
	commitAsyncStartResultWithContext(ctx, result, provider, store, &clock.Fake{Time: now}, events.Discard, 0, io.Discard, io.Discard, nil)
	row, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("get row: %v", err)
	}
	if row.Status == "closed" || row.Metadata["pending_create_claim"] != boolMetadata(true) {
		t.Fatalf("canceled start lost unconfirmed row despite stop failure: status=%q claim=%q", row.Status, row.Metadata["pending_create_claim"])
	}
	delete(provider.StopErrors, name)
	commitAsyncStartResultWithContext(ctx, result, provider, store, &clock.Fake{Time: now}, events.Discard, 0, io.Discard, io.Discard, nil)
	row, err = store.Get(info.ID)
	if err != nil {
		t.Fatalf("get recovered row: %v", err)
	}
	if row.Status != "closed" || provider.IsRunning(name) {
		t.Fatalf("confirmed teardown did not close row and stop runtime: status=%q running=%t", row.Status, provider.IsRunning(name))
	}
}

func TestStalePoolAsyncStartStopsUnattributedRuntime(t *testing.T) {
	for _, newerToken := range []bool{false, true} {
		name := "unattributed"
		if newerToken {
			name = "newer-token"
		}
		t.Run(name, func(t *testing.T) {
			store := beads.NewMemStore()
			provider := runtime.NewFake()
			now := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
			const template = "example/worker"
			info, err := createPoolSessionBeadWithAlias(store, template, nil, nil, now,
				poolSessionCreateIdentity{AgentName: "example/worker-1", Slot: 1}, "")
			if err != nil {
				t.Fatalf("create pool row: %v", err)
			}
			runtimeName := info.SessionNameMetadata
			rollbackPendingCreate(info, sessionFrontDoor(store), now, io.Discard)
			if row, err := store.Get(info.ID); err != nil || row.Status != "closed" {
				t.Fatalf("fixture rollback: row=%+v err=%v", row, err)
			}
			if err := provider.Start(context.Background(), runtimeName, runtime.Config{}); err != nil {
				t.Fatalf("late runtime start: %v", err)
			}
			if newerToken {
				if err := provider.SetMeta(runtimeName, "GC_INSTANCE_TOKEN", "newer-generation-token"); err != nil {
					t.Fatalf("set newer token: %v", err)
				}
			}
			result := startResult{
				prepared: preparedStart{candidate: startCandidate{info: info, tp: TemplateParams{SessionName: runtimeName, TemplateName: template, Command: "true"}}},
				outcome:  TraceOutcomeSessionExistsConverged, started: now, finished: now, provider: provider,
			}
			if commitAsyncStartResultWithContext(context.Background(), result, provider, store,
				&clock.Fake{Time: now}, events.Discard, 0, io.Discard, io.Discard, nil) {
				t.Fatal("stale async result reported committed")
			}
			if got := provider.IsRunning(runtimeName); got != newerToken {
				t.Fatalf("runtime running = %t, want %t for newer token = %t", got, newerToken, newerToken)
			}
		})
	}
}

func TestSyncPoolMintRespectsUnconfirmedIdentity(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)}
	provider := runtime.NewFake()
	const template = "pack/worker"
	const instance = "pack/worker-1"
	held, err := store.Create(beads.Bead{
		Title: "worker-1", Type: sessionBeadType, Labels: []string{sessionBeadLabel, "agent:" + instance},
		Metadata: map[string]string{
			"template": template, "session_name": "worker-held", "agent_name": instance,
			"pool_slot": "1", "state": string(sessionpkg.StateFailedCreate),
			"pending_create_claim": boolMetadata(true), poolManagedMetadataKey: boolMetadata(true),
		},
	})
	if err != nil {
		t.Fatalf("create held row: %v", err)
	}
	desired := map[string]TemplateParams{
		"legacy-worker-1": {TemplateName: template, InstanceName: instance, PoolSlot: 1, Command: "codex"},
	}
	var stderr bytes.Buffer
	syncSessionBeads("", store, desired, provider, allConfiguredDS(desired), nil, clk, &stderr, true)
	for _, row := range allSessionBeads(t, store) {
		if row.ID != held.ID && row.Status != "closed" {
			t.Fatalf("sync minted row %s beside unconfirmed holder %s; stderr=%s", row.ID, held.ID, stderr.String())
		}
	}
	if !strings.Contains(stderr.String(), "not creating pool session for "+instance) {
		t.Fatalf("missing held-identity diagnostic: %s", stderr.String())
	}
	if err := store.Close(held.ID); err != nil {
		t.Fatalf("close held row: %v", err)
	}
	stderr.Reset()
	syncSessionBeads("", store, desired, provider, allConfiguredDS(desired), nil, clk, &stderr, true)
	minted := 0
	for _, row := range allSessionBeads(t, store) {
		if row.ID == held.ID || row.Status == "closed" {
			continue
		}
		minted++
		if want := PoolSessionName(template, row.ID); row.Metadata["session_name"] != want {
			t.Fatalf("runtime name = %q, want %q", row.Metadata["session_name"], want)
		}
	}
	if minted != 1 {
		t.Fatalf("minted %d rows after holder closed, want 1; stderr=%s", minted, stderr.String())
	}
}

func TestPoolFailedStartStopFailureHoldsIdentity(t *testing.T) {
	store := beads.NewMemStore()
	provider := runtime.NewFake()
	now := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	const template = "example/worker"
	identity := poolSessionCreateIdentity{AgentName: "example/worker-1", Slot: 1}
	info, err := createPoolSessionBeadWithAlias(store, template, nil, newSessionBeadSnapshot(nil), now, identity, "")
	if err != nil {
		t.Fatalf("create pool row: %v", err)
	}
	name := info.SessionNameMetadata
	if err := provider.Start(context.Background(), name, runtime.Config{}); err != nil {
		t.Fatalf("start simulated runtime: %v", err)
	}
	provider.StopErrors = map[string]error{name: errors.New("provider unavailable")}
	result := startResult{
		prepared: preparedStart{candidate: startCandidate{info: info, tp: TemplateParams{SessionName: name, TemplateName: template, Command: "true"}}},
		err:      errors.New("start failed after provisioning"), outcome: TraceOutcomeProviderError,
		started: now, finished: now, rollbackPending: true, provider: provider,
	}
	commitStartFailure(result, sessionFrontDoor(store), &clock.Fake{Time: now}, events.Discard, 0, io.Discard, nil)
	row, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("read failed row: %v", err)
	}
	if row.Status == "closed" {
		t.Fatal("failed row closed before runtime teardown was confirmed")
	}
	open, err := loadSessionBeads(store)
	if err != nil {
		t.Fatalf("load open sessions: %v", err)
	}
	if _, err := createPoolSessionBeadWithAlias(store, template, nil, newSessionBeadSnapshot(open), now, identity, ""); err == nil {
		t.Fatalf("successor create error = %v, want identity held by unconfirmed row", err)
	}
}
