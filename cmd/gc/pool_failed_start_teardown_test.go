package main

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
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
