package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
	"github.com/gastownhall/gascity/internal/worker"
)

type sequencedDrainLivenessProvider struct {
	*runtime.Fake
	observeCalls  atomic.Int64
	unavailableAt int64
}

func (p *sequencedDrainLivenessProvider) ObserveLivenessWithError(name string, _ []string) (runtime.Liveness, error) {
	if p.observeCalls.Add(1) == p.unavailableAt {
		return runtime.Liveness{}, fmt.Errorf("drain liveness: %w", runtime.ErrRuntimeUnavailable)
	}
	running := p.IsRunning(name)
	return runtime.Liveness{Running: running, Alive: running}, nil
}

type countingWakeMetadataStore struct {
	*beads.MemStore
	singleCalls int
	batchCalls  int
}

type failingWakeMetadataStore struct {
	*beads.MemStore
	err error
}

func makeWakeBead(id string, meta map[string]string) beads.Bead {
	cloned := make(map[string]string, len(meta)+2)
	for k, v := range meta {
		cloned[k] = v
	}
	if cloned["provider"] == "" {
		cloned["provider"] = "claude"
	}
	if cloned["work_dir"] == "" {
		cloned["work_dir"] = "/tmp/gc-session-test"
	}
	return beads.Bead{ID: id, Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: cloned}
}

func TestAdvanceSessionDrains_LivenessUnavailableDefersOrdinaryDrainCompletion(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	sp := &sequencedDrainLivenessProvider{Fake: runtime.NewFake(), unavailableAt: 1}
	store := beads.NewMemStore()
	dt := newDrainTracker()
	if err := sp.Start(context.Background(), "test-session", runtime.Config{}); err != nil {
		t.Fatalf("start runtime: %v", err)
	}
	b, err := store.Create(beads.Bead{
		Title:  "test",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":         "test-session",
			"template":             "worker",
			"provider":             "claude",
			"work_dir":             t.TempDir(),
			"generation":           "3",
			"state":                "active",
			"wake_mode":            "fresh",
			"session_key":          "keep-session",
			"started_config_hash":  "keep-hash",
			"pending_create_claim": "true",
			"last_woke_at":         now.Add(-time.Minute).Format(time.RFC3339),
		},
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	ds := &drainState{startedAt: now.Add(-10 * time.Second), deadline: now.Add(20 * time.Second), reason: "idle", generation: 3}
	dt.set(b.ID, ds)
	before, err := store.Get(b.ID)
	if err != nil {
		t.Fatalf("Get before: %v", err)
	}

	advanceSessionDrainsWithSessionsTraced(dt, sp, store, infoLookupFromBeadLookup(func(id string) *beads.Bead {
		got, _ := store.Get(id)
		return &got
	}), map[string]wakeEvaluation{}, &config.City{}, clk, nil)

	if got := dt.get(b.ID); got != ds {
		t.Fatalf("drain tracker entry = %p, want retained %p", got, ds)
	}
	after, err := store.Get(b.ID)
	if err != nil {
		t.Fatalf("Get after: %v", err)
	}
	if !maps.Equal(after.Metadata, before.Metadata) {
		t.Fatalf("metadata mutated while liveness unavailable: before=%#v after=%#v", before.Metadata, after.Metadata)
	}
	if got := sp.CountCalls("Stop", "test-session"); got != 0 {
		t.Fatalf("Stop calls = %d, want 0", got)
	}
}

// wakeInfo projects a store-created fixture bead through the session front door
// (sessiontest.SeedBead runs the production codec at the store edge) instead of
// cracking it raw. These fixtures come from store.Create, which stamps
// Type="task"; the front door narrows on session shape, so the seed copy is
// retyped to a session bead. That retype is the ONLY projection delta —
// Info.Type becomes "session" instead of "task" — and no wake/drain consumer
// (preWakeCommit, completeDrain) reads Info.Type, so the returned Info is
// identical for every field they read (id, session_name, generation, wake_mode,
// continuation/identity metadata, created_at, closed) to the former raw
// InfoFromPersistedBead crack. b is taken by value, so the retype does
// not disturb the caller's bead or the store the consumer writes back to.
func wakeInfo(t *testing.T, b beads.Bead) sessionpkg.Info {
	t.Helper()
	return seedSessionInfo(b)
}

func (s *countingWakeMetadataStore) SetMetadata(id, key, value string) error {
	s.singleCalls++
	return s.MemStore.SetMetadata(id, key, value)
}

func (s *countingWakeMetadataStore) SetMetadataBatch(id string, kvs map[string]string) error {
	s.batchCalls++
	return s.MemStore.SetMetadataBatch(id, kvs)
}

func (s *failingWakeMetadataStore) SetMetadataBatch(_ string, _ map[string]string) error {
	return s.err
}

func TestPreWakeCommit(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := beads.NewMemStore()

	b, err := store.Create(beads.Bead{
		Title: "test-session",
		Metadata: map[string]string{
			"session_name": "test-worker",
			"template":     "worker",
			"generation":   "2",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	newGen, token, _, err := preWakeCommit(wakeInfo(t, b), sessionFrontDoor(store), clk)
	if err != nil {
		t.Fatalf("preWakeCommit: %v", err)
	}

	if newGen != 3 {
		t.Errorf("newGen = %d, want 3", newGen)
	}
	if token == "" {
		t.Error("expected non-empty token")
	}

	// Verify persisted in store.
	got, _ := store.Get(b.ID)
	if got.Metadata["generation"] != "3" {
		t.Errorf("stored generation = %q, want 3", got.Metadata["generation"])
	}
	if got.Metadata["instance_token"] != token {
		t.Errorf("stored token mismatch")
	}
	if got.Metadata["last_woke_at"] == "" {
		t.Error("expected last_woke_at to be set")
	}
	if got.Metadata["sleep_reason"] != "" {
		t.Error("expected sleep_reason to be cleared")
	}
	if got.Metadata["continuation_epoch"] != "1" {
		t.Errorf("stored continuation_epoch = %q, want 1", got.Metadata["continuation_epoch"])
	}
}

func TestPreWakeCommitUsesSingleBatchMetadataWrite(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := &countingWakeMetadataStore{MemStore: beads.NewMemStore()}

	b, err := store.Create(beads.Bead{
		Title: "test-session",
		Metadata: map[string]string{
			"session_name": "test-worker",
			"template":     "worker",
			"generation":   "2",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, _, _, err := preWakeCommit(wakeInfo(t, b), sessionFrontDoor(store), clk); err != nil {
		t.Fatalf("preWakeCommit: %v", err)
	}
	if store.batchCalls != 1 {
		t.Fatalf("batchCalls = %d, want 1", store.batchCalls)
	}
	if store.singleCalls != 0 {
		t.Fatalf("singleCalls = %d, want 0", store.singleCalls)
	}
}

func TestPreWakeCommit_InvalidName(t *testing.T) {
	clk := &clock.Fake{Time: time.Now()}
	store := beads.NewMemStore()

	b, _ := store.Create(beads.Bead{
		Title: "bad-session",
		Metadata: map[string]string{
			"session_name": "../bad",
			"template":     "worker",
		},
	})

	_, _, _, err := preWakeCommit(wakeInfo(t, b), sessionFrontDoor(store), clk)
	if err == nil {
		t.Error("expected error for invalid session_name")
	}
}

func TestPreWakeCommit_BumpsContinuationEpochForFreshWake(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := beads.NewMemStore()

	b, err := store.Create(beads.Bead{
		Title: "fresh-session",
		Metadata: map[string]string{
			"session_name":       "fresh-worker",
			"template":           "worker",
			"generation":         "2",
			"continuation_epoch": "3",
			"wake_mode":          "fresh",
			"last_woke_at":       now.Add(-time.Minute).UTC().Format(time.RFC3339),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, _, _, err := preWakeCommit(wakeInfo(t, b), sessionFrontDoor(store), clk); err != nil {
		t.Fatalf("preWakeCommit: %v", err)
	}
	got, _ := store.Get(b.ID)
	if got.Metadata["continuation_epoch"] != "4" {
		t.Fatalf("continuation_epoch = %q, want 4", got.Metadata["continuation_epoch"])
	}
}

func TestPreWakeCommit_FreshModeClearsPreviousConversationMetadata(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := beads.NewMemStore()

	b, err := store.Create(beads.Bead{
		Title: "fresh-session",
		Metadata: map[string]string{
			"session_name":               "fresh-worker",
			"template":                   "worker",
			"generation":                 "2",
			"continuation_epoch":         "3",
			"continuation_reset_pending": "true",
			"wake_mode":                  "fresh",
			"session_key":                "old-provider-conversation",
			"started_config_hash":        "old-core-hash",
			"started_live_hash":          "old-live-hash",
			"live_hash":                  "old-live-hash",
			"startup_dialog_verified":    "true",
			"last_woke_at":               now.Add(-time.Minute).UTC().Format(time.RFC3339),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, _, fold, err := preWakeCommit(wakeInfo(t, b), sessionFrontDoor(store), clk)
	if err != nil {
		t.Fatalf("preWakeCommit: %v", err)
	}
	got, _ := store.Get(b.ID)
	for _, key := range []string{
		"session_key",
		"started_config_hash",
		"started_live_hash",
		"live_hash",
		"startup_dialog_verified",
	} {
		if got.Metadata[key] != "" {
			t.Errorf("%s = %q, want cleared for wake_mode=fresh", key, got.Metadata[key])
		}
		// The returned fold is the in-memory carrier (the caller folds it onto its
		// coherent Info snapshot); it must clear each fresh-wake conversation key.
		if v, ok := fold[key]; !ok || v != "" {
			t.Errorf("fold %s = %q (present=%v), want cleared for wake_mode=fresh", key, v, ok)
		}
	}
	if got.Metadata["continuation_epoch"] != "4" {
		t.Fatalf("continuation_epoch = %q, want bumped to 4", got.Metadata["continuation_epoch"])
	}
	if got.Metadata["continuation_reset_pending"] != "" {
		t.Fatalf("continuation_reset_pending = %q, want consumed", got.Metadata["continuation_reset_pending"])
	}
}

func TestPreWakeCommit_ResumeModePreservesPreviousConversationMetadata(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := beads.NewMemStore()

	b, err := store.Create(beads.Bead{
		Title: "resume-session",
		Metadata: map[string]string{
			"session_name":            "resume-worker",
			"template":                "worker",
			"generation":              "2",
			"continuation_epoch":      "3",
			"wake_mode":               "resume",
			"session_key":             "resume-conversation",
			"started_config_hash":     "resume-core-hash",
			"started_live_hash":       "resume-live-hash",
			"live_hash":               "resume-live-hash",
			"startup_dialog_verified": "true",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	newGen, token, _, err := preWakeCommit(wakeInfo(t, b), sessionFrontDoor(store), clk)
	if err != nil {
		t.Fatalf("preWakeCommit: %v", err)
	}
	got, _ := store.Get(b.ID)
	if newGen != 3 {
		t.Fatalf("newGen = %d, want 3", newGen)
	}
	if token == "" {
		t.Fatal("expected non-empty token")
	}
	want := map[string]string{
		"session_key":                "resume-conversation",
		"started_config_hash":        "resume-core-hash",
		"started_live_hash":          "resume-live-hash",
		"live_hash":                  "resume-live-hash",
		"startup_dialog_verified":    "true",
		"instance_token":             token,
		"generation":                 "3",
		"continuation_epoch":         "3",
		"continuation_reset_pending": "",
		"detached_at":                "",
		"last_woke_at":               now.UTC().Format(time.RFC3339),
		"sleep_reason":               "",
		"sleep_intent":               "",
	}
	for key, value := range want {
		if got.Metadata[key] != value {
			t.Errorf("%s = %q, want preserved %q", key, got.Metadata[key], value)
		}
	}
}

func TestPreWakeCommit_FreshModeTraceLogsClearedProviderMetadata(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := beads.NewMemStore()
	t.Setenv("GC_TMUX_TRACE", "1")

	b, err := store.Create(beads.Bead{
		Title: "fresh-session",
		Metadata: map[string]string{
			"session_name":            "fresh-worker",
			"template":                "worker",
			"generation":              "2",
			"wake_mode":               "fresh",
			"session_key":             "old-provider-conversation",
			"started_config_hash":     "old-core-hash",
			"started_live_hash":       "old-live-hash",
			"live_hash":               "old-live-hash",
			"startup_dialog_verified": "true",
			// Priming markers share the fresh-wake reset (S19 Stage 2); set them
			// so the trace log lists them among the cleared keys.
			"primed_at":            "2026-03-08T11:00:00Z",
			"priming_attempted_at": "2026-03-08T11:00:00Z",
			"prompt_hash":          "abc123",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	var logBuf bytes.Buffer
	prevWriter := log.Writer()
	prevFlags := log.Flags()
	prevPrefix := log.Prefix()
	log.SetOutput(&logBuf)
	log.SetFlags(0)
	log.SetPrefix("")
	t.Cleanup(func() {
		log.SetOutput(prevWriter)
		log.SetFlags(prevFlags)
		log.SetPrefix(prevPrefix)
	})

	if _, _, _, err := preWakeCommit(wakeInfo(t, b), sessionFrontDoor(store), clk); err != nil {
		t.Fatalf("preWakeCommit: %v", err)
	}

	gotLog := strings.TrimSpace(logBuf.String())
	if !strings.Contains(gotLog, "[WAKE-TRACE] preWakeCommit session=fresh-worker wake_mode=fresh cleared_provider_metadata=") {
		t.Fatalf("trace log = %q, want fresh-wake trace prefix", gotLog)
	}
	for _, key := range sessionpkg.FreshWakeConversationResetKeys() {
		if !strings.Contains(gotLog, key) {
			t.Fatalf("trace log = %q, want key %q present", gotLog, key)
		}
	}
}

func TestPreWakeCommit_FreshModeTraceSilentWhenTraceDisabled(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := beads.NewMemStore()
	b, err := store.Create(beads.Bead{
		Title: "fresh-session",
		Metadata: map[string]string{
			"session_name":            "fresh-worker",
			"template":                "worker",
			"generation":              "2",
			"wake_mode":               "fresh",
			"session_key":             "old-provider-conversation",
			"started_config_hash":     "old-core-hash",
			"started_live_hash":       "old-live-hash",
			"live_hash":               "old-live-hash",
			"startup_dialog_verified": "true",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	var logBuf bytes.Buffer
	prevWriter := log.Writer()
	prevFlags := log.Flags()
	prevPrefix := log.Prefix()
	log.SetOutput(&logBuf)
	log.SetFlags(0)
	log.SetPrefix("")
	t.Cleanup(func() {
		log.SetOutput(prevWriter)
		log.SetFlags(prevFlags)
		log.SetPrefix(prevPrefix)
	})

	if _, _, _, err := preWakeCommit(wakeInfo(t, b), sessionFrontDoor(store), clk); err != nil {
		t.Fatalf("preWakeCommit: %v", err)
	}
	if strings.TrimSpace(logBuf.String()) != "" {
		t.Fatalf("trace log = %q, want empty when GC_TMUX_TRACE is unset", logBuf.String())
	}
}

func TestPreWakeCommit_FreshModeTraceSilentWhenNothingCleared(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := beads.NewMemStore()
	t.Setenv("GC_TMUX_TRACE", "1")
	b, err := store.Create(beads.Bead{
		Title: "fresh-session",
		Metadata: map[string]string{
			"session_name": "fresh-worker",
			"template":     "worker",
			"generation":   "2",
			"wake_mode":    "fresh",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	var logBuf bytes.Buffer
	prevWriter := log.Writer()
	prevFlags := log.Flags()
	prevPrefix := log.Prefix()
	log.SetOutput(&logBuf)
	log.SetFlags(0)
	log.SetPrefix("")
	t.Cleanup(func() {
		log.SetOutput(prevWriter)
		log.SetFlags(prevFlags)
		log.SetPrefix(prevPrefix)
	})

	if _, _, _, err := preWakeCommit(wakeInfo(t, b), sessionFrontDoor(store), clk); err != nil {
		t.Fatalf("preWakeCommit: %v", err)
	}
	if strings.TrimSpace(logBuf.String()) != "" {
		t.Fatalf("trace log = %q, want empty when no provider metadata is cleared", logBuf.String())
	}
}

func TestPreWakeCommit_ResumeModeTraceSilent(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := beads.NewMemStore()
	t.Setenv("GC_TMUX_TRACE", "1")
	b, err := store.Create(beads.Bead{
		Title: "resume-session",
		Metadata: map[string]string{
			"session_name":            "resume-worker",
			"template":                "worker",
			"generation":              "2",
			"wake_mode":               "resume",
			"session_key":             "resume-conversation",
			"started_config_hash":     "resume-core-hash",
			"started_live_hash":       "resume-live-hash",
			"live_hash":               "resume-live-hash",
			"startup_dialog_verified": "true",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	var logBuf bytes.Buffer
	prevWriter := log.Writer()
	prevFlags := log.Flags()
	prevPrefix := log.Prefix()
	log.SetOutput(&logBuf)
	log.SetFlags(0)
	log.SetPrefix("")
	t.Cleanup(func() {
		log.SetOutput(prevWriter)
		log.SetFlags(prevFlags)
		log.SetPrefix(prevPrefix)
	})

	if _, _, _, err := preWakeCommit(wakeInfo(t, b), sessionFrontDoor(store), clk); err != nil {
		t.Fatalf("preWakeCommit: %v", err)
	}
	if strings.TrimSpace(logBuf.String()) != "" {
		t.Fatalf("trace log = %q, want empty for resume wake", logBuf.String())
	}
}

func TestPreWakeCommit_FreshModeTraceSilentOnStoreFailure(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := &failingWakeMetadataStore{
		MemStore: beads.NewMemStore(),
		err:      os.ErrPermission,
	}
	t.Setenv("GC_TMUX_TRACE", "1")
	b, err := store.Create(beads.Bead{
		Title: "fresh-session",
		Metadata: map[string]string{
			"session_name":            "fresh-worker",
			"template":                "worker",
			"generation":              "2",
			"wake_mode":               "fresh",
			"session_key":             "old-provider-conversation",
			"started_config_hash":     "old-core-hash",
			"started_live_hash":       "old-live-hash",
			"live_hash":               "old-live-hash",
			"startup_dialog_verified": "true",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	var logBuf bytes.Buffer
	prevWriter := log.Writer()
	prevFlags := log.Flags()
	prevPrefix := log.Prefix()
	log.SetOutput(&logBuf)
	log.SetFlags(0)
	log.SetPrefix("")
	t.Cleanup(func() {
		log.SetOutput(prevWriter)
		log.SetFlags(prevFlags)
		log.SetPrefix(prevPrefix)
	})

	if _, _, _, err := preWakeCommit(wakeInfo(t, b), sessionFrontDoor(store), clk); err == nil {
		t.Fatal("preWakeCommit: expected error")
	}
	if strings.TrimSpace(logBuf.String()) != "" {
		t.Fatalf("trace log = %q, want empty when metadata commit fails", logBuf.String())
	}
}

func TestPreWakeCommit_BumpsContinuationEpochForPendingReset(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := beads.NewMemStore()

	b, err := store.Create(beads.Bead{
		Title: "reset-session",
		Metadata: map[string]string{
			"session_name":               "reset-worker",
			"template":                   "worker",
			"generation":                 "2",
			"continuation_epoch":         "5",
			"continuation_reset_pending": "true",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, _, _, err := preWakeCommit(wakeInfo(t, b), sessionFrontDoor(store), clk); err != nil {
		t.Fatalf("preWakeCommit: %v", err)
	}
	got, _ := store.Get(b.ID)
	if got.Metadata["continuation_epoch"] != "6" {
		t.Fatalf("continuation_epoch = %q, want 6", got.Metadata["continuation_epoch"])
	}
	if got.Metadata["continuation_reset_pending"] != "" {
		t.Fatalf("continuation_reset_pending = %q, want empty", got.Metadata["continuation_reset_pending"])
	}
}

func TestValidateWorkDir(t *testing.T) {
	// Valid: use temp dir.
	dir := t.TempDir()
	if err := validateWorkDir(dir); err != nil {
		t.Errorf("valid dir: %v", err)
	}

	// Invalid: non-existent.
	if err := validateWorkDir("/nonexistent/path/xyz"); err == nil {
		t.Error("expected error for nonexistent path")
	}

	// Invalid: file, not directory.
	f := dir + "/file.txt"
	if err := writeTestFile(f); err != nil {
		t.Fatal(err)
	}
	if err := validateWorkDir(f); err == nil {
		t.Error("expected error for file path")
	}
}

func writeTestFile(path string) error {
	return os.WriteFile(path, []byte("test"), 0o644)
}

func TestVerifiedStop_MatchingToken(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	oldLive := liveSubagentsForKill
	liveSubagentsForKill = func(context.Context, worker.Handle) ([]worker.InFlightSubagent, error) {
		return []worker.InFlightSubagent{{AgentID: "helper", Description: "Inspect drain state"}}, nil
	}
	t.Cleanup(func() { liveSubagentsForKill = oldLive })
	var observed bytes.Buffer
	oldLogWriter := log.Writer()
	log.SetOutput(&observed)
	t.Cleanup(func() { log.SetOutput(oldLogWriter) })
	mgr := newSessionManagerWithConfig("", store, sp, nil)
	info, err := mgr.CreateSession(context.Background(), sessionpkg.CreateOptions{Template: "worker", Title: "Worker", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", Env: nil, Resume: sessionpkg.ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	session, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}

	err = verifiedStop(sessiontest.SeedBead(t, session), store, sp, nil)
	if err != nil {
		t.Errorf("verifiedStop with matching token: %v", err)
	}
	if sp.IsRunning(info.SessionName) {
		t.Error("expected session to be stopped")
	}
	if got := observed.String(); !strings.Contains(got, "session drain timeout") || !strings.Contains(got, "helper") {
		t.Fatalf("autonomous-stop observation = %q", got)
	}
}

func TestVerifiedStop_MismatchedToken(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := newSessionManagerWithConfig("", store, sp, nil)
	info, err := mgr.CreateSession(context.Background(), sessionpkg.CreateOptions{Template: "worker", Title: "Worker", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", Env: nil, Resume: sessionpkg.ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.SetMetadata(info.ID, "instance_token", "new-token"); err != nil {
		t.Fatalf("SetMetadata(instance_token): %v", err)
	}
	if err := sp.SetMeta(info.SessionName, "GC_INSTANCE_TOKEN", "old-token"); err != nil {
		t.Fatalf("SetMeta(GC_INSTANCE_TOKEN): %v", err)
	}
	session, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}

	err = verifiedStop(sessiontest.SeedBead(t, session), store, sp, nil)
	if err == nil {
		t.Error("expected error for mismatched token")
	}
	if !sp.IsRunning(info.SessionName) {
		t.Error("session should NOT be stopped on token mismatch")
	}
}

func TestVerifiedStop_NoToken(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := newSessionManagerWithConfig("", store, sp, nil)
	info, err := mgr.CreateSession(context.Background(), sessionpkg.CreateOptions{Template: "worker", Title: "Worker", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", Env: nil, Resume: sessionpkg.ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.SetMetadata(info.ID, "instance_token", ""); err != nil {
		t.Fatalf("SetMetadata(instance_token): %v", err)
	}
	session, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}

	err = verifiedStop(sessiontest.SeedBead(t, session), store, sp, nil)
	if err != nil {
		t.Errorf("verifiedStop with no token: %v", err)
	}
}

// verifiedStopTokenFixture creates a running session whose bead expects
// instance token "tok-a" and whose runtime token read answers readErr.
func verifiedStopTokenFixture(t *testing.T, readErr error) (beads.Store, *runtime.Fake, sessionpkg.Info) {
	t.Helper()
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := newSessionManagerWithConfig("", store, sp, nil)
	info, err := mgr.CreateSession(context.Background(), sessionpkg.CreateOptions{Template: "worker", Title: "Worker", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.SetMetadata(info.ID, "instance_token", "tok-a"); err != nil {
		t.Fatalf("SetMetadata(instance_token): %v", err)
	}
	sp.GetMetaErrors[info.SessionName] = map[string]error{"GC_INSTANCE_TOKEN": readErr}
	session, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	return store, sp, sessiontest.SeedBead(t, session)
}

// An empty token that came with a read error must not pass the fence: the
// drain-timeout kill is skipped and retried, and is not mistaken for a stale
// drain (errTokenMismatch would cancel it).
func TestVerifiedStopDefersOnUnverifiableToken(t *testing.T) {
	store, sp, info := verifiedStopTokenFixture(t, fmt.Errorf("show-environment timed out: %w", runtime.ErrRuntimeUnavailable))

	err := verifiedStop(info, store, sp, nil)
	if !errors.Is(err, errTokenUnverifiable) || errors.Is(err, errTokenMismatch) {
		t.Fatalf("verifiedStop error = %v, want errTokenUnverifiable (not errTokenMismatch)", err)
	}
	if !sp.IsRunning(info.SessionNameMetadata) {
		t.Fatal("verifiedStop killed a session whose instance token could not be read")
	}
}

// A runtime with no metadata store has no token by construction: the kill
// proceeds, or exec kills would become impossible.
func TestVerifiedStopProceedsOnMetaUnsupported(t *testing.T) {
	store, sp, info := verifiedStopTokenFixture(t, fmt.Errorf("exec get-meta: %w", runtime.ErrMetaUnsupported))

	if err := verifiedStop(info, store, sp, nil); err != nil {
		t.Fatalf("verifiedStop error = %v, want nil on ErrMetaUnsupported", err)
	}
	if sp.IsRunning(info.SessionNameMetadata) {
		t.Fatal("verifiedStop did not stop a session on a meta-less runtime")
	}
}

func TestBeginSessionDrain(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	sp := runtime.NewFake()
	dt := newDrainTracker()

	_ = sp.Start(context.Background(), "test-session", runtime.Config{})

	session := makeWakeBead("b1", map[string]string{
		"session_name": "test-session",
		"generation":   "5",
	})

	if transitioned := beginSessionDrainInfo(sessiontest.SeedBead(t, session), sp, dt, "idle", clk, 30*time.Second); !transitioned {
		t.Fatal("first beginSessionDrain = false, want true (state transition)")
	}

	ds := dt.get("b1")
	if ds == nil {
		t.Fatal("expected drain state")
	}
	if ds.reason != "idle" {
		t.Errorf("reason = %q, want idle", ds.reason)
	}
	if ds.generation != 5 {
		t.Errorf("generation = %d, want 5", ds.generation)
	}
	if ds.deadline != now.Add(30*time.Second) {
		t.Errorf("deadline = %v, want %v", ds.deadline, now.Add(30*time.Second))
	}
}

func TestBeginSessionDrain_AlreadyDraining(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	sp := runtime.NewFake()
	dt := newDrainTracker()

	_ = sp.Start(context.Background(), "test-session", runtime.Config{})

	session := makeWakeBead("b1", map[string]string{
		"session_name": "test-session",
		"generation":   "5",
	})

	if transitioned := beginSessionDrainInfo(sessiontest.SeedBead(t, session), sp, dt, "idle", clk, 30*time.Second); !transitioned {
		t.Fatal("first beginSessionDrain = false, want true (state transition)")
	}
	if transitioned := beginSessionDrainInfo(sessiontest.SeedBead(t, session), sp, dt, "config-drift", clk, 60*time.Second); transitioned {
		t.Error("second beginSessionDrain = true, want false (already draining)")
	}

	// Second drain should not overwrite first.
	ds := dt.get("b1")
	if ds.reason != "idle" {
		t.Errorf("reason = %q, want idle (should not be overwritten)", ds.reason)
	}
}

func TestCancelSessionDrain(t *testing.T) {
	sp := runtime.NewFake()
	dt := newDrainTracker()
	dt.set("b1", &drainState{
		reason:     "idle",
		generation: 5,
	})

	session := makeWakeBead("b1", map[string]string{
		"generation": "5",
	})

	if !cancelSessionDrainInfo(sessiontest.SeedBead(t, session), sp, dt) {
		t.Error("expected cancel to succeed")
	}
	if dt.get("b1") != nil {
		t.Error("drain should be removed after cancel")
	}
}

func TestCancelSessionDrain_ClearsAck(t *testing.T) {
	sp := runtime.NewFake()
	_ = sp.Start(context.Background(), "test-session", runtime.Config{})
	_ = sp.SetMeta("test-session", "GC_DRAIN_ACK", "1")

	dt := newDrainTracker()
	dt.set("b1", &drainState{
		reason:     "idle",
		generation: 5,
		ackSet:     true,
	})

	session := makeWakeBead("b1", map[string]string{
		"session_name": "test-session",
		"generation":   "5",
	})

	if !cancelSessionDrainInfo(sessiontest.SeedBead(t, session), sp, dt) {
		t.Error("expected cancel to succeed")
	}
	// GC_DRAIN_ACK should be cleared.
	ack, _ := sp.GetMeta("test-session", "GC_DRAIN_ACK")
	if ack != "" {
		t.Errorf("GC_DRAIN_ACK = %q, want empty (should be cleared on cancel)", ack)
	}
}

func TestCancelSessionDrain_GenerationMismatch(t *testing.T) {
	sp := runtime.NewFake()
	dt := newDrainTracker()
	dt.set("b1", &drainState{
		reason:     "idle",
		generation: 5,
	})

	session := makeWakeBead("b1", map[string]string{
		"generation": "6", // re-woken
	})

	if cancelSessionDrainInfo(sessiontest.SeedBead(t, session), sp, dt) {
		t.Error("cancel should fail when generation doesn't match")
	}
}

func TestCancelSessionDrain_NonCancelableReason(t *testing.T) {
	sp := runtime.NewFake()
	dt := newDrainTracker()
	dt.set("b1", &drainState{
		reason:     "orphaned",
		generation: 5,
	})

	session := makeWakeBead("b1", map[string]string{
		"generation": "5",
	})

	if cancelSessionDrainInfo(sessiontest.SeedBead(t, session), sp, dt) {
		t.Error("cancel should fail for non-cancelable drain reason")
	}
	if ds := dt.get("b1"); ds == nil || ds.reason != "orphaned" {
		t.Errorf("non-cancelable drain should remain, got %+v", ds)
	}
}

// infoLookupFromBeadLookup adapts a raw *beads.Bead lookup to the typed Info
// lookup the drain scan consumes. The drain tests still carry raw beads; the
// reconciler builds its Info lookup directly from the coherent infoByID
// snapshot instead.
func infoLookupFromBeadLookup(sessionLookup func(id string) *beads.Bead) func(id string) (sessionpkg.Info, bool) {
	return func(id string) (sessionpkg.Info, bool) {
		b := sessionLookup(id)
		if b == nil {
			return sessionpkg.Info{}, false
		}
		// The looked-up bead has a non-empty id and is session-shaped; project it
		// through the shared front-door seeder (type-stamp is a no-op / unread).
		return seedSessionInfo(*b), true
	}
}

func TestAdvanceSessionDrains_ProcessExited(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	sp := runtime.NewFake()
	store := beads.NewMemStore()
	dt := newDrainTracker()

	// No session running (process exited).
	b, _ := store.Create(beads.Bead{
		Title: "test",
		Metadata: map[string]string{
			"session_name": "test-session",
			"template":     "worker",
			"generation":   "3",
			"state":        "active",
			"pool_slot":    "1",
		},
	})

	dt.set(b.ID, &drainState{
		startedAt:  now.Add(-10 * time.Second),
		deadline:   now.Add(20 * time.Second),
		reason:     "idle",
		generation: 3,
	})

	cfg := &config.City{}

	advanceSessionDrainsWithSessionsTraced(dt, sp, store, infoLookupFromBeadLookup(func(id string) *beads.Bead {
		got, _ := store.Get(id)
		return &got
	}), map[string]wakeEvaluation{}, cfg, clk, nil)

	// Drain should be cleaned up.
	if dt.get(b.ID) != nil {
		t.Error("drain should be removed after process exit")
	}

	// Metadata should be updated.
	got, _ := store.Get(b.ID)
	if got.Metadata["state"] != "asleep" {
		t.Errorf("state = %q, want asleep", got.Metadata["state"])
	}
	if got.Metadata["sleep_reason"] != "idle" {
		t.Errorf("sleep_reason = %q, want idle", got.Metadata["sleep_reason"])
	}
}

func TestAdvanceSessionDrains_Timeout(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	sp := runtime.NewFake()
	store := beads.NewMemStore()
	dt := newDrainTracker()

	// Session is still running.
	_ = sp.Start(context.Background(), "test-session", runtime.Config{})

	b, _ := store.Create(beads.Bead{
		Title:  "test",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "test-session",
			"template":     "worker",
			"provider":     "claude",
			"work_dir":     t.TempDir(),
			"generation":   "3",
			"state":        "active",
		},
	})

	// Drain deadline already passed.
	dt.set(b.ID, &drainState{
		startedAt:  now.Add(-60 * time.Second),
		deadline:   now.Add(-10 * time.Second),
		reason:     "pool-excess",
		generation: 3,
	})

	cfg := &config.City{}

	advanceSessionDrainsWithSessionsTraced(dt, sp, store, infoLookupFromBeadLookup(func(id string) *beads.Bead {
		got, _ := store.Get(id)
		return &got
	}), map[string]wakeEvaluation{}, cfg, clk, nil)

	// Should have force-stopped.
	if sp.IsRunning("test-session") {
		t.Error("session should be force-stopped after deadline")
	}
	if dt.get(b.ID) != nil {
		t.Error("drain should be removed after timeout")
	}
}

func TestAdvanceSessionDrains_WakeReasonsReappear(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	sp := runtime.NewFake()
	store := beads.NewMemStore()
	dt := newDrainTracker()

	// Session is still running.
	_ = sp.Start(context.Background(), "test-session", runtime.Config{})

	b, _ := store.Create(beads.Bead{
		Title:  "test",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "test-session",
			"template":     "worker",
			"provider":     "claude",
			"work_dir":     t.TempDir(),
			"generation":   "3",
		},
	})

	dt.set(b.ID, &drainState{
		startedAt:  now.Add(-10 * time.Second),
		deadline:   now.Add(20 * time.Second),
		reason:     "idle", // NOT config-drift, so cancelable
		generation: 3,
	})

	// A desired pool slot still has WakeConfig, which should cancel the drain.
	cfg := &config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(1)}}}

	advanceSessionDrainsWithSessionsTraced(dt, sp, store, infoLookupFromBeadLookup(func(id string) *beads.Bead {
		got, _ := store.Get(id)
		return &got
	}), map[string]wakeEvaluation{
		b.ID: {Reasons: []WakeReason{WakeConfig}},
	}, cfg, clk, nil)

	// Drain should be canceled — wake reasons reappeared.
	if dt.get(b.ID) != nil {
		t.Error("drain should be canceled when wake reasons reappear")
	}
	// Session should still be running.
	if !sp.IsRunning("test-session") {
		t.Error("session should still be running after drain cancel")
	}
}

func TestAdvanceSessionDrains_DeferredInterrupt_CanceledBeforeSignal(t *testing.T) {
	// Simulates a false-orphan: beginSessionDrain is called but the drain
	// is canceled on the very next tick when wake reasons reappear.
	// The interrupt (Ctrl-C) should never reach the session.
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	sp := runtime.NewFake()
	store := beads.NewMemStore()
	dt := newDrainTracker()

	_ = sp.Start(context.Background(), "test-session", runtime.Config{})

	b, _ := store.Create(beads.Bead{
		Title:  "test",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "test-session",
			"template":     "worker",
			"provider":     "claude",
			"work_dir":     t.TempDir(),
			"generation":   "3",
		},
	})

	// beginSessionDrain no longer sends Ctrl-C immediately.
	beginSessionDrainInfo(sessiontest.SeedBead(t, makeWakeBead(b.ID, map[string]string{
		"session_name": "test-session",
		"generation":   "3",
	})), sp, dt, "orphaned", clk, 30*time.Second)

	// No interrupt should have been sent yet.
	for _, c := range sp.Calls {
		if c.Method == "Interrupt" {
			t.Fatal("beginSessionDrain should not send interrupt immediately")
		}
	}

	// Simulate next tick: wake reasons reappear (store recovered) → cancel drain.
	cfg := &config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(1)}}}
	advanceSessionDrainsWithSessionsTraced(dt, sp, store, infoLookupFromBeadLookup(func(id string) *beads.Bead {
		got, _ := store.Get(id)
		return &got
	}), map[string]wakeEvaluation{
		b.ID: {Reasons: []WakeReason{WakeConfig}},
	}, cfg, clk, nil)

	// Orphaned drains are non-cancelable because the session is leaving the
	// desired set. The drain survives and receives its deferred signal.
	ds := dt.get(b.ID)
	if ds == nil {
		t.Fatal("orphaned drain should not be canceled by wake reasons")
	}
	if !ds.ackSet {
		t.Error("drain-ack should have been set during advance")
	}
	if !ds.followUp {
		t.Error("drain follow-up tick should be requested when deferred drain-ack is set")
	}
	// Verify GC_DRAIN_ACK was set (not Ctrl-C)
	ack, _ := sp.GetMeta("test-session", "GC_DRAIN_ACK")
	if ack != "1" {
		t.Errorf("GC_DRAIN_ACK = %q, want \"1\"", ack)
	}
	for _, c := range sp.Calls {
		if c.Method == "Interrupt" {
			t.Error("Interrupt (Ctrl-C) should never be sent — use GC_DRAIN_ACK instead")
		}
	}
}

func TestAdvanceSessionDrains_OrphanedDrainCanceledForAssignedWork(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	sp := runtime.NewFake()
	store := beads.NewMemStore()
	dt := newDrainTracker()

	if err := sp.Start(context.Background(), "test-session", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := sp.SetMeta("test-session", "GC_DRAIN_ACK", "1"); err != nil {
		t.Fatalf("SetMeta(GC_DRAIN_ACK): %v", err)
	}
	b, err := store.Create(beads.Bead{
		Title:  "test",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "test-session",
			"template":     "worker",
			"provider":     "claude",
			"work_dir":     t.TempDir(),
			"generation":   "3",
			"state":        "active",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	dt.set(b.ID, &drainState{
		startedAt:  now.Add(-10 * time.Second),
		deadline:   now.Add(20 * time.Second),
		reason:     "orphaned",
		generation: 3,
		ackSet:     true,
	})
	advanceSessionDrainsWithSessionsTraced(
		dt,
		sp,
		store,
		infoLookupFromBeadLookup(func(id string) *beads.Bead {
			got, _ := store.Get(id)
			return &got
		}),
		map[string]wakeEvaluation{
			b.ID: {
				Reasons: []WakeReason{WakeWork},
				Reason:  "assigned-work",
			},
		},
		&config.City{Agents: []config.Agent{{Name: "worker"}}},
		clk,
		nil,
	)

	if ds := dt.get(b.ID); ds != nil {
		t.Fatalf("drain = %+v, want canceled for assigned work", ds)
	}
	if ack, _ := sp.GetMeta("test-session", "GC_DRAIN_ACK"); ack != "" {
		t.Fatalf("GC_DRAIN_ACK = %q, want cleared after assigned-work cancellation", ack)
	}
	if !sp.IsRunning("test-session") {
		t.Fatal("session should stay running after assigned-work cancellation")
	}
	for _, call := range sp.Calls {
		if call.Method == "Interrupt" || call.Method == "Stop" {
			t.Fatalf("runtime call %s should not happen after assigned-work cancellation; calls=%#v", call.Method, sp.Calls)
		}
	}
}

func TestAdvanceSessionDrains_NoWakeDrainCanceledForAssignedWork(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	sp := runtime.NewFake()
	store := beads.NewMemStore()
	dt := newDrainTracker()

	if err := sp.Start(context.Background(), "test-session", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := sp.SetMeta("test-session", "GC_DRAIN_ACK", "1"); err != nil {
		t.Fatalf("SetMeta(GC_DRAIN_ACK): %v", err)
	}
	b, err := store.Create(beads.Bead{
		Title:  "test",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "test-session",
			"template":     "worker",
			"provider":     "claude",
			"work_dir":     t.TempDir(),
			"generation":   "3",
			"state":        "active",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	dt.set(b.ID, &drainState{
		startedAt:  now.Add(-10 * time.Second),
		deadline:   now.Add(20 * time.Second),
		reason:     "no-wake-reason",
		generation: 3,
		ackSet:     true,
	})
	advanceSessionDrainsWithSessionsTraced(
		dt,
		sp,
		store,
		infoLookupFromBeadLookup(func(id string) *beads.Bead {
			got, _ := store.Get(id)
			return &got
		}),
		map[string]wakeEvaluation{
			b.ID: {
				Reasons: []WakeReason{WakeWork},
				Reason:  "assigned-work",
			},
		},
		&config.City{Agents: []config.Agent{{Name: "worker"}}},
		clk,
		nil,
	)

	if ds := dt.get(b.ID); ds != nil {
		t.Fatalf("drain = %+v, want canceled for assigned work", ds)
	}
	if ack, _ := sp.GetMeta("test-session", "GC_DRAIN_ACK"); ack != "" {
		t.Fatalf("GC_DRAIN_ACK = %q, want cleared after assigned-work cancellation", ack)
	}
	if !sp.IsRunning("test-session") {
		t.Fatal("session should stay running after assigned-work cancellation")
	}
	for _, call := range sp.Calls {
		if call.Method == "Interrupt" || call.Method == "Stop" {
			t.Fatalf("runtime call %s should not happen after assigned-work cancellation; calls=%#v", call.Method, sp.Calls)
		}
	}
}

func TestAssignedWorkDrainReasonCancelable(t *testing.T) {
	tests := []struct {
		reason string
		want   bool
	}{
		{reason: "orphaned", want: true},
		{reason: "no-wake-reason", want: true},
		{reason: "idle", want: false},
		{reason: "config-drift", want: false},
		{reason: "suspended", want: false},
		{reason: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.reason, func(t *testing.T) {
			if got := assignedWorkDrainReasonCancelable(tt.reason); got != tt.want {
				t.Fatalf("assignedWorkDrainReasonCancelable(%q) = %v, want %v", tt.reason, got, tt.want)
			}
		})
	}
}

func TestAdvanceSessionDrains_DeferredInterrupt_CancelableNoSignal(t *testing.T) {
	// For cancelable drains (no-wake-reason, idle), verify the drain is
	// canceled before the deferred interrupt fires.
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	sp := runtime.NewFake()
	store := beads.NewMemStore()
	dt := newDrainTracker()

	_ = sp.Start(context.Background(), "test-session", runtime.Config{})

	b, _ := store.Create(beads.Bead{
		Title: "test",
		Metadata: map[string]string{
			"session_name": "test-session",
			"template":     "worker",
			"generation":   "3",
		},
	})

	// Begin a cancelable drain (no-wake-reason).
	beginSessionDrainInfo(sessiontest.SeedBead(t, makeWakeBead(b.ID, map[string]string{
		"session_name": "test-session",
		"generation":   "3",
	})), sp, dt, "no-wake-reason", clk, 30*time.Second)

	// No interrupt yet.
	for _, c := range sp.Calls {
		if c.Method == "Interrupt" {
			t.Fatal("beginSessionDrain should not send interrupt immediately")
		}
	}

	// Simulate next tick: wake reasons reappear → cancel drain before interrupt.
	cfg := &config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(1)}}}
	advanceSessionDrainsWithSessionsTraced(dt, sp, store, infoLookupFromBeadLookup(func(id string) *beads.Bead {
		got, _ := store.Get(id)
		return &got
	}), map[string]wakeEvaluation{
		b.ID: {Reasons: []WakeReason{WakeConfig}},
	}, cfg, clk, nil)

	// Drain should be canceled — no-wake-reason is cancelable.
	if dt.get(b.ID) != nil {
		t.Error("no-wake-reason drain should be canceled when wake reasons reappear")
	}

	// No drain signal should have been sent — cancel happened first.
	for _, c := range sp.Calls {
		if c.Method == "Interrupt" {
			t.Error("Interrupt should not fire for a drain that was canceled before advance")
		}
		if c.Method == "SetMeta" && c.Name == "test-session" {
			t.Error("GC_DRAIN_ACK should not be set for a drain that was canceled before advance")
		}
	}
}

func TestAdvanceSessionDrains_ConfigDriftCancelableOnPendingWake(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	sp := runtime.NewFake()
	store := beads.NewMemStore()
	dt := newDrainTracker()

	// Session is still running.
	_ = sp.Start(context.Background(), "test-session", runtime.Config{})

	b, _ := store.Create(beads.Bead{
		Title: "test",
		Metadata: map[string]string{
			"session_name": "test-session",
			"template":     "worker",
			"generation":   "3",
		},
	})

	dt.set(b.ID, &drainState{
		startedAt:  now.Add(-10 * time.Second),
		deadline:   now.Add(20 * time.Second),
		reason:     "config-drift",
		generation: 3,
	})

	cfg := &config.City{Agents: []config.Agent{{Name: "worker"}}}
	advanceSessionDrainsWithSessionsTraced(dt, sp, store, infoLookupFromBeadLookup(func(id string) *beads.Bead {
		got, _ := store.Get(id)
		return &got
	}), map[string]wakeEvaluation{
		b.ID: {Reasons: []WakeReason{WakePending}},
	}, cfg, clk, nil)

	if dt.get(b.ID) != nil {
		t.Error("config-drift drain should be canceled by a pending wake")
	}
}

func TestAdvanceSessionDrains_TimeoutTokenMismatch(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	sp := runtime.NewFake()
	store := beads.NewMemStore()
	dt := newDrainTracker()

	// Session is running with a different token (re-woken by different incarnation).
	_ = sp.Start(context.Background(), "test-session", runtime.Config{})
	_ = sp.SetMeta("test-session", "GC_INSTANCE_TOKEN", "new-token")

	b, _ := store.Create(beads.Bead{
		Title:  "test",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":   "test-session",
			"template":       "worker",
			"provider":       "claude",
			"work_dir":       t.TempDir(),
			"generation":     "3",
			"instance_token": "old-token", // stale token
		},
	})

	// Drain deadline already passed.
	dt.set(b.ID, &drainState{
		startedAt:  now.Add(-60 * time.Second),
		deadline:   now.Add(-10 * time.Second),
		reason:     "pool-excess",
		generation: 3,
	})

	cfg := &config.City{}

	advanceSessionDrainsWithSessionsTraced(dt, sp, store, infoLookupFromBeadLookup(func(id string) *beads.Bead {
		got, _ := store.Get(id)
		return &got
	}), map[string]wakeEvaluation{}, cfg, clk, nil)

	// Drain should be canceled (stale token), session still running.
	if dt.get(b.ID) != nil {
		t.Error("drain should be removed after token mismatch")
	}
	if !sp.IsRunning("test-session") {
		t.Error("session should still be running after token mismatch")
	}
	// Metadata should NOT be updated to asleep.
	got, _ := store.Get(b.ID)
	if got.Metadata["state"] == "asleep" {
		t.Error("state should not be asleep after failed stop")
	}
}

// A token that cannot be read is not a replacement: the timed-out drain is kept
// for retry (not canceled as stale), nothing is stopped, and the retry trace
// says token_unverifiable. The trace error omits the read error's text, which
// IsSessionGone would read as gone.
func TestAdvanceSessionDrains_TimeoutUnverifiableTokenKeepsDrain(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	sp := runtime.NewFake()
	store := beads.NewMemStore()
	dt := newDrainTracker()

	_ = sp.Start(context.Background(), "test-session", runtime.Config{})
	sp.GetMetaErrors["test-session"] = map[string]error{"GC_INSTANCE_TOKEN": errors.New("no tmux server running")}

	b, _ := store.Create(beads.Bead{
		Title:  "test",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":   "test-session",
			"template":       "worker",
			"provider":       "claude",
			"work_dir":       t.TempDir(),
			"generation":     "3",
			"instance_token": "tok-a",
		},
	})
	dt.set(b.ID, &drainState{
		startedAt:  now.Add(-60 * time.Second),
		deadline:   now.Add(-10 * time.Second),
		reason:     "pool-excess",
		generation: 3,
	})
	trace := newPoolDesiredStateTestTrace("worker")

	advanceSessionDrainsWithSessionsTraced(dt, sp, store, infoLookupFromBeadLookup(func(id string) *beads.Bead {
		got, _ := store.Get(id)
		return &got
	}), map[string]wakeEvaluation{}, &config.City{}, clk, trace)

	if dt.get(b.ID) == nil {
		t.Fatal("drain was canceled on an unverifiable token, want it kept for retry")
	}
	if !sp.IsRunning("test-session") || sp.CountCalls("Stop", "test-session") != 0 {
		t.Fatal("drain timeout stopped a session whose instance token could not be read")
	}
	var retry *SessionReconcilerTraceRecord
	for i := range trace.records {
		if trace.records[i].SiteCode == TraceSiteDrainTimeout {
			retry = &trace.records[i]
		}
	}
	if retry == nil {
		t.Fatal("no drain-timeout trace record")
	}
	if retry.OutcomeCode != TraceOutcomeRetry || retry.Fields["token"] != "token_unverifiable" {
		t.Fatalf("drain-timeout trace = outcome %q token %v, want retry with token=token_unverifiable", retry.OutcomeCode, retry.Fields["token"])
	}
	if msg, _ := retry.Fields["error"].(string); runtime.IsSessionGone(errors.New(msg)) {
		t.Fatalf("drain-timeout trace error %q reads as gone to IsSessionGone", msg)
	}
}

func TestCompleteDrain_ClearsLastWokeAt(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := beads.NewMemStore()

	b, _ := store.Create(beads.Bead{
		Title: "test",
		Metadata: map[string]string{
			"session_name": "test-session",
			"last_woke_at": now.Add(-10 * time.Second).UTC().Format(time.RFC3339),
		},
	})

	ds := &drainState{reason: "idle"}
	completeDrain(wakeInfo(t, b), sessionFrontDoor(store), ds, clk)

	got, _ := store.Get(b.ID)
	if got.Metadata["last_woke_at"] != "" {
		t.Errorf("last_woke_at should be cleared after drain, got %q", got.Metadata["last_woke_at"])
	}
	if got.Metadata["state"] != "asleep" {
		t.Errorf("state = %q, want asleep", got.Metadata["state"])
	}
	if got.Metadata["sleep_reason"] != "idle" {
		t.Errorf("sleep_reason = %q, want idle", got.Metadata["sleep_reason"])
	}
}

func TestCompleteDrain_FreshModeClearsIdentity(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := beads.NewMemStore()

	b, _ := store.Create(beads.Bead{
		Title: "test",
		Metadata: map[string]string{
			"session_name":        "test-session",
			"wake_mode":           "fresh",
			"session_key":         "stale-key",
			"started_config_hash": "stale-hash",
			"last_woke_at":        now.Add(-10 * time.Second).UTC().Format(time.RFC3339),
		},
	})

	ds := &drainState{reason: "idle"}
	completeDrain(wakeInfo(t, b), sessionFrontDoor(store), ds, clk)

	got, _ := store.Get(b.ID)
	if got.Metadata["session_key"] != "" {
		t.Errorf("session_key = %q, want cleared for wake_mode=fresh", got.Metadata["session_key"])
	}
	if got.Metadata["started_config_hash"] != "" {
		t.Errorf("started_config_hash = %q, want cleared for wake_mode=fresh", got.Metadata["started_config_hash"])
	}
	if got.Metadata["continuation_reset_pending"] != "true" {
		t.Errorf("continuation_reset_pending = %q, want true", got.Metadata["continuation_reset_pending"])
	}
	if got.Metadata["last_woke_at"] != "" {
		t.Errorf("last_woke_at should be cleared, got %q", got.Metadata["last_woke_at"])
	}
}

func TestCompleteDrain_ResumeModePreservesIdentity(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := beads.NewMemStore()

	b, _ := store.Create(beads.Bead{
		Title: "test",
		Metadata: map[string]string{
			"session_name":        "test-session",
			"wake_mode":           "resume",
			"session_key":         "resume-key",
			"started_config_hash": "resume-hash",
			"last_woke_at":        now.Add(-10 * time.Second).UTC().Format(time.RFC3339),
		},
	})

	ds := &drainState{reason: "idle"}
	completeDrain(wakeInfo(t, b), sessionFrontDoor(store), ds, clk)

	got, _ := store.Get(b.ID)
	if got.Metadata["session_key"] != "resume-key" {
		t.Errorf("session_key = %q, want preserved for wake_mode=resume", got.Metadata["session_key"])
	}
	if got.Metadata["started_config_hash"] != "resume-hash" {
		t.Errorf("started_config_hash = %q, want preserved for wake_mode=resume", got.Metadata["started_config_hash"])
	}
	if got.Metadata["last_woke_at"] != "" {
		t.Errorf("last_woke_at should be cleared, got %q", got.Metadata["last_woke_at"])
	}
}

func TestCompleteDrain_ClearsPendingCreateClaim(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	store := beads.NewMemStore()

	b, _ := store.Create(beads.Bead{
		Title: "test",
		Metadata: map[string]string{
			"session_name":         "test-session",
			"pending_create_claim": "true",
		},
	})

	ds := &drainState{reason: "idle"}
	completeDrain(wakeInfo(t, b), sessionFrontDoor(store), ds, clk)

	got, _ := store.Get(b.ID)
	if got.Metadata["pending_create_claim"] != "" {
		t.Errorf("pending_create_claim = %q, want cleared after drain completion", got.Metadata["pending_create_claim"])
	}
}

func TestAdvanceSessionDrains_CancelsForReadyWait(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	sp := runtime.NewFake()
	store := beads.NewMemStore()
	dt := newDrainTracker()

	_ = sp.Start(context.Background(), "test-session", runtime.Config{})

	b, _ := store.Create(beads.Bead{
		Title: "test",
		Metadata: map[string]string{
			"session_name": "test-session",
			"template":     "worker",
			"generation":   "3",
		},
	})

	dt.set(b.ID, &drainState{
		startedAt:  now.Add(-10 * time.Second),
		deadline:   now.Add(20 * time.Second),
		reason:     "wait-hold",
		generation: 3,
	})

	advanceSessionDrainsWithSessionsTraced(dt, sp, store, infoLookupFromBeadLookup(func(id string) *beads.Bead {
		got, _ := store.Get(id)
		return &got
	}), map[string]wakeEvaluation{
		b.ID: {Reasons: []WakeReason{WakeWait}},
	}, &config.City{}, clk, nil)

	if dt.get(b.ID) != nil {
		t.Fatal("drain should be canceled when a wait becomes ready mid-drain")
	}
	if !sp.IsRunning("test-session") {
		t.Fatal("session should remain running after wait-based drain cancellation")
	}
}

func TestAdvanceSessionDrains_ClearsIdleProbeOnCompletion(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	sp := runtime.NewFake()
	store := beads.NewMemStore()
	dt := newDrainTracker()

	b, err := store.Create(beads.Bead{
		Title: "test",
		Metadata: map[string]string{
			"session_name": "test-session",
			"template":     "worker",
			"generation":   "3",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	dt.set(b.ID, &drainState{
		startedAt:  now.Add(-10 * time.Second),
		deadline:   now.Add(20 * time.Second),
		reason:     "config-drift",
		generation: 3,
	})
	if probe := dt.startIdleProbe(b.ID); probe == nil {
		t.Fatal("expected idle probe to start")
	}

	advanceSessionDrainsWithSessionsTraced(dt, sp, store, infoLookupFromBeadLookup(func(id string) *beads.Bead {
		got, _ := store.Get(id)
		return &got
	}), map[string]wakeEvaluation{}, &config.City{}, clk, nil)

	if dt.get(b.ID) != nil {
		t.Fatal("drain should be removed after completion")
	}
	if _, ok := dt.idleProbe(b.ID); ok {
		t.Fatal("idle probe should be cleared when the drain completes")
	}
}

func TestDrainTracker_FinishIdleProbeIgnoresStaleProbe(t *testing.T) {
	dt := newDrainTracker()
	first := dt.startIdleProbe("bead-1")
	if first == nil {
		t.Fatal("expected first idle probe to start")
	}
	dt.clearIdleProbe("bead-1")

	second := dt.startIdleProbe("bead-1")
	if second == nil {
		t.Fatal("expected replacement idle probe to start")
	}

	dt.finishIdleProbe("bead-1", first, true, time.Now().UTC())
	probe, ok := dt.idleProbe("bead-1")
	if !ok {
		t.Fatal("expected current probe to remain registered")
	}
	if probe.ready {
		t.Fatal("stale probe completion should not mark the replacement probe ready")
	}

	dt.finishIdleProbe("bead-1", second, true, time.Now().UTC())
	probe, ok = dt.idleProbe("bead-1")
	if !ok || !probe.ready || !probe.success {
		t.Fatalf("replacement probe should complete successfully, got ok=%v probe=%+v", ok, probe)
	}
}

// TestClearMissingIdleProbes guards the Step-6c conversion of
// clearMissingIdleProbes off the raw beadByID pointer map onto the typed
// infoByID snapshot. Both carry exactly the working set's ids, so presence in
// infoByID must decide retention identically to a non-nil beadByID entry: a
// probe whose session is still in the snapshot is kept; a probe whose session
// has left the working set is cleared. Info values are irrelevant — only key
// presence matters (a closed-but-present session keeps its probe here; the
// drain path clears it elsewhere).
func TestClearMissingIdleProbes(t *testing.T) {
	dt := newDrainTracker()
	for _, id := range []string{"present", "also-present", "missing"} {
		if dt.startIdleProbe(id) == nil {
			t.Fatalf("expected idle probe to start for %q", id)
		}
	}

	infoByID := map[string]sessionpkg.Info{
		"present":      {ID: "present"},
		"also-present": {ID: "also-present", Closed: true},
	}

	clearMissingIdleProbes(dt, infoByID)

	if _, ok := dt.idleProbe("present"); !ok {
		t.Fatal("probe for a session still in the snapshot must be retained")
	}
	if _, ok := dt.idleProbe("also-present"); !ok {
		t.Fatal("probe for a closed-but-present session must be retained (presence, not Info value, decides)")
	}
	if _, ok := dt.idleProbe("missing"); ok {
		t.Fatal("probe for a session absent from the snapshot must be cleared")
	}

	// Nil-tracker fast path: the reconciler may run without a drain tracker in
	// reduced configurations, so the call must be a safe no-op.
	clearMissingIdleProbes(nil, infoByID)
}

type s1AckBeforeWriteFailureOps struct{ drainOps }

func (o s1AckBeforeWriteFailureOps) setDrainAck(string) error {
	return errors.New("provider refused ACK before writing")
}

// Canonical cancellation keeps the same runtime alive, so admission must reopen
// for that runtime after a successful cancellation, including failed ACK writes.
func TestS1CanonicalDrainCancellationReopensAdmission(t *testing.T) {
	for _, failAck := range []bool{false, true} {
		t.Run(map[bool]string{false: "successful-ack", true: "failed-before-write"}[failAck], func(t *testing.T) {
			fixture := newS1FileAdmissionFixture(t, "open", "")
			sp := runtime.NewFake()
			if err := sp.Start(context.Background(), "worker-1", runtime.Config{}); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			tracker := newDrainTracker()
			tracker.set(fixture.seat.ID, &drainState{startedAt: now.Add(-10 * time.Second), deadline: now.Add(20 * time.Second), reason: "wait-hold", generation: 7})
			oldRelease, oldPoke := drainAckReleaseHeldClaims, drainAckPokeController
			t.Cleanup(func() { drainAckReleaseHeldClaims, drainAckPokeController = oldRelease, oldPoke })
			drainAckReleaseHeldClaims = func(string, string, io.Writer) error {
				return releaseUnexecutedClaimsOnDrainAck(fixture.city, fixture.cfg, fixture.store, nil, fixture.seat, drainAckReleaseBudget, io.Discard)
			}
			drainAckPokeController = func(string, reconcilekey.Key) error { return nil }
			ops := newDrainOps(sp)
			if failAck {
				ops = s1AckBeforeWriteFailureOps{ops}
			}
			code, err := doRuntimeDrainAckResult(ops, fixture.city, "worker-1", "worker-1", fixture.seat.ID, true, io.Discard, io.Discard)
			if (err != nil) != failAck || (code != 0) != failAck {
				t.Fatalf("wrong ACK stage: code=%d err=%v", code, err)
			}
			_, cell, _, readErr := sessionFrontDoor(fixture.store).ExecutionEvidence(fixture.seat.ID)
			if readErr != nil || cell.Drain == nil {
				t.Fatalf("ACK did not establish drain gate: %v", readErr)
			}
			tracker.get(fixture.seat.ID).admissionAttempt = cell.Drain.Attempt
			advanceSessionDrainsWithSessionsTraced(tracker, sp, fixture.store, infoLookupFromBeadLookup(func(id string) *beads.Bead {
				row, err := fixture.store.Get(id)
				if err != nil {
					return nil
				}
				return &row
			}), map[string]wakeEvaluation{fixture.seat.ID: {Reasons: []WakeReason{WakeWait}}}, fixture.cfg, &clock.Fake{Time: now}, nil)
			if tracker.get(fixture.seat.ID) != nil {
				t.Fatal("canonical cancellation did not remove tracker")
			}
			row, err := fixture.store.Get(fixture.seat.ID)
			if err != nil || row.Status == "closed" || !sp.IsRunning("worker-1") {
				t.Fatalf("cancellation did not preserve runtime: status=%s running=%v err=%v", row.Status, sp.IsRunning("worker-1"), err)
			}
			writer, _, err := beads.ResolveConditionalWriter(fixture.store)
			if err != nil || writer == nil {
				t.Fatalf("work writer unavailable: %v", err)
			}
			status, owner := "in_progress", "worker-1"
			if err := writer.UpdateIfMatch(fixture.work.ID, fixture.work.Revision, beads.UpdateOpts{Status: &status, Assignee: &owner}); err != nil {
				t.Fatal(err)
			}
			fresh, err := fixture.store.Get(fixture.work.ID)
			if err != nil {
				t.Fatal(err)
			}
			if code, output := s1AdmissionResult(t, fixture, fresh, hookRecordStartedClaim); code != 0 || !bytes.Contains([]byte(output), []byte(`"action":"work"`)) {
				t.Fatalf("same-incarnation work stayed refused after canonical cancellation: code=%d output=%s", code, output)
			}
		})
	}
}

func TestS1ControllerDrainIssueAndTimeoutUseCapturedAttempt(t *testing.T) {
	for _, scenario := range []string{"current-ack", "timeout", "cancel-before-timeout", "failed-timeout-retry", "unreadable-token"} {
		t.Run(scenario, func(t *testing.T) {
			env := newReconcilerTestEnv()
			mem := beads.NewMemStore()
			if err := beads.StampOpenedStore(mem, "memory", gate.Require, nil, nil); err != nil {
				t.Fatal(err)
			}
			interleaved := &s1ControllerStopInterleaveStore{MemStore: mem, beforeAccept: true}
			env.store = interleaved
			env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}, Beads: config.BeadsConfig{ConditionalWrites: "require"}}
			if err := env.sp.Start(context.Background(), "worker", runtime.Config{}); err != nil {
				t.Fatal(err)
			}
			seat := env.createSessionBead("worker", "worker")
			env.markSessionActive(&seat)
			for k, v := range map[string]string{"GC_INSTANCE_TOKEN": "test-token", "GC_SESSION_ID": seat.ID} {
				if err := env.sp.SetMeta("worker", k, v); err != nil {
					t.Fatal(err)
				}
			}
			front := sessionFrontDoor(interleaved)
			baseFront := sessionFrontDoor(mem)
			info := env.sessionInfo(seat.ID)
			if !beginSessionDrainInfo(info, env.sp, env.dt, "wait-hold", env.clk, time.Second, front) {
				t.Fatal("qualified controller issue failed")
			}
			ds := env.dt.get(seat.ID)
			_, cell, enrolled, err := front.ExecutionEvidence(seat.ID)
			if err != nil || !enrolled || ds.admissionAttempt == "" || cell.Drain == nil || cell.Drain.Attempt != ds.admissionAttempt || cell.Drain.Committed {
				t.Fatalf("issue did not close distinct controller intent: ds=%+v cell=%+v err=%v", ds, cell, err)
			}
			fence := sessionpkg.ExecutionFence{InstanceToken: "test-token", Generation: "1"}
			var admissionErr error
			interleaved.intervene = func() {
				if err := baseFront.CancelDrainAdmission(seat.ID, fence, ds.admissionAttempt); err != nil {
					t.Errorf("cancellation-before-timeout: %v", err)
				}
				admissionErr = baseFront.RecordStartedClaim(seat.ID, fence, sessionpkg.WorkStartedEntry{Bead: "fresh-work"})
			}
			if scenario != "cancel-before-timeout" {
				interleaved.fired = true
			}
			if scenario == "failed-timeout-retry" {
				env.sp.StopErrors["worker"] = errors.New("provider unavailable")
			}
			if scenario == "unreadable-token" {
				env.sp.GetMetaErrors["worker"] = map[string]error{"GC_INSTANCE_TOKEN": errors.New("token read unavailable")}
			}
			if scenario != "current-ack" {
				env.clk.Time = env.clk.Time.Add(2 * time.Second)
			}
			lookup := infoLookupFromBeadLookup(func(id string) *beads.Bead {
				row, err := mem.Get(id)
				if err != nil {
					return nil
				}
				return &row
			})
			advance := func(wake map[string]wakeEvaluation) {
				advanceSessionDrainsWithSessionsTraced(env.dt, env.sp, interleaved, lookup, wake, env.cfg, env.clk, nil)
			}
			advance(nil)
			if scenario == "cancel-before-timeout" {
				if !interleaved.fired || admissionErr != nil || !env.sp.IsRunning("worker") {
					t.Fatalf("cancellation winner stopped or could not resume: fired=%v err=%v running=%v", interleaved.fired, admissionErr, env.sp.IsRunning("worker"))
				}
				return
			}
			if scenario == "current-ack" {
				nonce, err := env.sp.GetMeta("worker", drainAdmissionAttemptKey)
				if err != nil || nonce != ds.admissionAttempt {
					t.Fatalf("controller publication unbound: %q %v", nonce, err)
				}
				seat, err = mem.Get(seat.ID)
				if err != nil {
					t.Fatal(err)
				}
				reconcileS1StopCompletion(t, env, []beads.Bead{seat}, map[string]int{"worker": 0}, newDrainOps(env.sp))
				if env.sp.IsRunning("worker") {
					t.Fatalf("valid controller ACK did not finish: %s", env.stderr.String())
				}
				return
			}
			if scenario == "failed-timeout-retry" || scenario == "unreadable-token" {
				if !env.sp.IsRunning("worker") {
					t.Fatal("failed/unreadable stop killed runtime")
				}
				if err := baseFront.CancelDrainAdmission(seat.ID, fence, ds.admissionAttempt); !errors.Is(err, sessionpkg.ErrDrainStopAccepted) {
					t.Fatalf("accepted failed timeout reopened: %v", err)
				}
				if err := baseFront.RecordStartedClaim(seat.ID, fence, sessionpkg.WorkStartedEntry{Bead: "new-work"}); !errors.Is(err, sessionpkg.ErrExecutionDraining) {
					t.Fatalf("failed timeout admitted work: %v", err)
				}
				delete(env.sp.StopErrors, "worker")
				delete(env.sp.GetMetaErrors, "worker")
				advance(map[string]wakeEvaluation{seat.ID: {Reasons: []WakeReason{WakeWait}}})
			}
			if env.sp.IsRunning("worker") {
				t.Fatal("accepted timeout did not complete or retry")
			}
		})
	}
}

func TestS1DelayedControllerPublicationCannotAdoptReplacementDrain(t *testing.T) {
	env := newReconcilerTestEnv()
	mem := beads.NewMemStore()
	env.store = mem
	if err := beads.StampOpenedStore(mem, "memory", gate.Require, nil, nil); err != nil {
		t.Fatal(err)
	}
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}, Beads: config.BeadsConfig{ConditionalWrites: "require"}}
	if err := env.sp.Start(context.Background(), "worker", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	seat := env.createSessionBead("worker", "worker")
	env.markSessionActive(&seat)
	for k, v := range map[string]string{"GC_INSTANCE_TOKEN": "test-token", "GC_SESSION_ID": seat.ID} {
		if err := env.sp.SetMeta("worker", k, v); err != nil {
			t.Fatal(err)
		}
	}
	front := sessionFrontDoor(mem)
	info := env.sessionInfo(seat.ID)
	fence := sessionpkg.ExecutionFence{InstanceToken: "test-token", Generation: "1"}
	if !beginSessionDrainInfo(info, env.sp, env.dt, "wait-hold", env.clk, time.Second, front) {
		t.Fatal("first issue failed")
	}
	old := env.dt.get(seat.ID)
	env.clk.Time = env.clk.Time.Add(2 * time.Second)
	paused := &s1PausedAckProvider{Provider: env.sp, paused: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() {
		select {
		case <-paused.release:
		default:
			close(paused.release)
		}
	})
	lookup := infoLookupFromBeadLookup(func(id string) *beads.Bead {
		row, err := mem.Get(id)
		if err != nil {
			return nil
		}
		return &row
	})
	done := make(chan struct{})
	go func() {
		advanceSessionDrainsWithSessionsTraced(env.dt, paused, mem, lookup, nil, env.cfg, env.clk, nil)
		close(done)
	}()
	select {
	case <-paused.paused:
	case <-time.After(2 * time.Second):
		t.Fatal("controller publication barrier not reached")
	}
	if !cancelSessionDrainInfo(info, env.sp, env.dt, front) {
		t.Fatal("canonical cancellation did not win")
	}
	if err := front.RecordStartedClaim(seat.ID, fence, sessionpkg.WorkStartedEntry{Bead: "fresh-work"}); err != nil {
		t.Fatal(err)
	}
	if !beginSessionDrainInfo(info, env.sp, env.dt, "wait-hold", env.clk, time.Minute, front) {
		t.Fatal("replacement issue failed")
	}
	replacement := env.dt.get(seat.ID)
	if replacement.admissionAttempt == old.admissionAttempt {
		t.Fatal("replacement reused canceled identity")
	}
	close(paused.release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("old publication did not finish")
	}
	if old.ackSet || env.dt.get(seat.ID) != replacement || !env.sp.IsRunning("worker") {
		t.Fatal("old publication adopted/deleted replacement intent or timed out resumed runtime")
	}
	_, cell, _, err := front.ExecutionEvidence(seat.ID)
	if err != nil || cell.Drain == nil || cell.Drain.Attempt != replacement.admissionAttempt || cell.Drain.Accepted || cell.Drain.Committed || !cell.Names("", "fresh-work") {
		t.Fatalf("replacement or active evidence changed: %+v %v", cell, err)
	}
	seat, err = mem.Get(seat.ID)
	if err != nil {
		t.Fatal(err)
	}
	env.reconcileWithPoolDesiredAndDrainOps([]beads.Bead{seat}, map[string]int{"worker": 0}, newDrainOps(env.sp))
	if !env.sp.IsRunning("worker") {
		t.Fatal("stale controller ACK stopped replacement before its own acceptance")
	}
}

func TestS1RecoveredCancellationRejectsStalePublishedAttempt(t *testing.T) {
	for _, assigned := range []bool{false, true} {
		for _, stale := range []bool{false, true} {
			t.Run(fmt.Sprintf("assigned-%v/stale-%v", assigned, stale), func(t *testing.T) {
				env := newReconcilerTestEnv()
				if err := beads.StampOpenedStore(env.store, "memory", gate.Require, nil, nil); err != nil {
					t.Fatal(err)
				}
				seat := env.createSessionBead("worker", "worker")
				env.markSessionActive(&seat)
				front := sessionFrontDoor(env.store)
				info, _, _, err := front.ExecutionEvidence(seat.ID)
				if err != nil {
					t.Fatal(err)
				}
				fence := sessionpkg.ExecutionFence{InstanceToken: info.InstanceToken, Generation: info.Generation}
				published, err := front.BeginControllerDrainAdmission(seat.ID, fence)
				if err != nil {
					t.Fatal(err)
				}
				current := published
				if stale {
					current, err = front.BeginControllerDrainAdmission(seat.ID, fence)
					if err != nil {
						t.Fatal(err)
					}
				}
				for k, v := range map[string]string{reconcilerDrainAckSourceKey: reconcilerDrainAckSourceValue, reconcilerDrainAckReasonKey: "no-wake-reason", reconcilerDrainAckGenerationKey: info.Generation, drainAdmissionAttemptKey: published, "GC_DRAIN_ACK": "1", "GC_SESSION_ID": seat.ID, "GC_INSTANCE_TOKEN": info.InstanceToken} {
					if err := env.sp.SetMeta("worker", k, v); err != nil {
						t.Fatal(err)
					}
				}
				if err := publishDrainAckFrame(env.sp, "worker", info, reconcilerDrainAckSourceValue, "no-wake-reason", published); err != nil {
					t.Fatal(err)
				}
				var canceled bool
				if assigned {
					canceled = cancelRecoveredDrainForAssignedWorkInfo(info, env.sp, "worker", front)
				} else {
					canceled = cancelRecoveredReconcilerAckedDrainInfo(info, env.sp, "worker", front)
				}
				_, cell, _, err := front.ExecutionEvidence(seat.ID)
				if err != nil {
					t.Fatal(err)
				}
				ack, err := env.sp.GetMeta("worker", "GC_DRAIN_ACK")
				if err != nil {
					t.Fatal(err)
				}
				if stale {
					if canceled || cell.Drain == nil || cell.Drain.Attempt != current || ack != "1" {
						t.Fatalf("stale provider proof canceled replacement: canceled=%v gate=%+v ack=%q", canceled, cell.Drain, ack)
					}
				} else if !canceled || cell.Drain != nil || ack != "" {
					t.Fatalf("exact recovered intent did not cancel: canceled=%v gate=%+v ack=%q", canceled, cell.Drain, ack)
				}
			})
		}
	}
}

type s1RecoveryPublicationInterleave struct {
	*runtime.Fake
	onRead         func()
	blockedAttempt string
	written        chan struct{}
	resume         chan struct{}
	blocked        atomic.Bool
}

func (p *s1RecoveryPublicationInterleave) GetMeta(name, key string) (string, error) {
	value, err := p.Fake.GetMeta(name, key)
	if err == nil && (key == reconcilerDrainAckReasonKey || key == "GC_DRAIN_ADMISSION_FRAME") && p.onRead != nil {
		callback := p.onRead
		p.onRead = nil
		callback()
	}
	return value, err
}

func (p *s1RecoveryPublicationInterleave) SetMeta(name, key, value string) error {
	if err := p.Fake.SetMeta(name, key, value); err != nil {
		return err
	}
	if key == drainAdmissionAttemptKey && value == p.blockedAttempt && p.blocked.CompareAndSwap(false, true) {
		close(p.written)
		<-p.resume
	}
	return nil
}

func TestS1RecoveredCancellationCannotMixReasonAndReplacementAttempt(t *testing.T) {
	for _, reason := range []string{"config-drift", "no-wake-reason"} {
		t.Run(reason, func(t *testing.T) {
			env := newReconcilerTestEnv()
			if err := beads.StampOpenedStore(env.store, "memory", gate.Require, nil, nil); err != nil {
				t.Fatal(err)
			}
			seat := env.createSessionBead("worker", "worker")
			env.markSessionActive(&seat)
			front := sessionFrontDoor(env.store)
			info, _, _, err := front.ExecutionEvidence(seat.ID)
			if err != nil {
				t.Fatal(err)
			}
			fence := sessionpkg.ExecutionFence{InstanceToken: info.InstanceToken, Generation: info.Generation}
			original, err := front.BeginControllerDrainAdmission(seat.ID, fence)
			if err != nil {
				t.Fatal(err)
			}
			p := &s1RecoveryPublicationInterleave{Fake: env.sp, written: make(chan struct{}), resume: make(chan struct{})}
			for k, v := range map[string]string{"GC_SESSION_ID": seat.ID, "GC_INSTANCE_TOKEN": info.InstanceToken} {
				if err := p.SetMeta("worker", k, v); err != nil {
					t.Fatal(err)
				}
			}
			if err := setReconcilerDrainAckMetadata(p, "worker", &drainState{reason: reason, generation: 1, admissionAttempt: original}, front); err != nil {
				t.Fatal(err)
			}
			replacement := ""
			published := make(chan error, 1)
			p.onRead = func() {
				var err error
				replacement, err = front.BeginControllerDrainAdmission(seat.ID, fence)
				if err != nil {
					t.Fatal(err)
				}
				p.blockedAttempt = replacement
				go func() {
					published <- setReconcilerDrainAckMetadata(p, "worker", &drainState{reason: "orphaned", generation: 1, admissionAttempt: replacement}, front)
				}()
				select {
				case <-p.written:
				case <-time.After(3 * time.Second):
					t.Fatal("replacement publisher did not reach attempt barrier")
				}
			}
			defer func() {
				select {
				case <-p.resume:
				default:
					close(p.resume)
				}
			}()
			var canceled bool
			if reason == "config-drift" {
				canceled, err = cancelConfigDriftDrainInfoResult(info, p, newDrainTracker(), front)
			} else {
				canceled = cancelRecoveredReconcilerAckedDrainInfo(info, p, "worker", front)
			}
			close(p.resume)
			select {
			case <-published:
			case <-time.After(3 * time.Second):
				t.Fatal("replacement publisher did not terminate")
			}
			if replacement == "" {
				t.Fatal("recovery did not exercise replacement interleave")
			}
			_, cell, _, readErr := front.ExecutionEvidence(seat.ID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if canceled || cell.Drain == nil || cell.Drain.Attempt != replacement {
				t.Fatalf("mixed old reason/new attempt canceled unrelated replacement: canceled=%v gate=%+v err=%v", canceled, cell.Drain, err)
			}
		})
	}
}
