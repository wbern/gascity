package main

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/worker"
)

func TestHandoffRemoteRefusesLiveSubagentsUnlessForced(t *testing.T) {
	old := liveSubagentsForKill
	liveSubagentsForKill = func(context.Context, worker.Handle) ([]worker.InFlightSubagent, error) {
		return []worker.InFlightSubagent{{AgentID: "helper", Description: "Investigate make check slowness", StartedAt: time.Now().Add(-time.Minute)}}, nil
	}
	t.Cleanup(func() { liveSubagentsForKill = old })
	store, rec, sp := beads.NewMemStore(), events.NewFake(), runtime.NewFake()
	if err := sp.Start(context.Background(), "target", runtime.Config{Command: "true"}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := doHandoffRemote(store, store, rec, sp, "target", "target", "sender", []string{"handoff"}, &stdout, &stderr); code != 1 {
		t.Fatalf("code = %d, want refusal", code)
	}
	if !sp.IsRunning("target") || !bytes.Contains(stdout.Bytes(), []byte("--force")) || !bytes.Contains(stdout.Bytes(), []byte("helper")) {
		t.Fatalf("refusal output=%q running=%v", stdout.String(), sp.IsRunning("target"))
	}
	stdout.Reset()
	if code := doHandoffRemoteWithForce(store, store, rec, sp, "target", "target", "sender", []string{"handoff"}, true, &stdout, &stderr); code != 0 || sp.IsRunning("target") {
		t.Fatalf("force code=%d running=%v stderr=%s", code, sp.IsRunning("target"), stderr.String())
	}
}
