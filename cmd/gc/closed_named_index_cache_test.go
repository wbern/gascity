package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

func onDemandNamedSessionCity() *config.City {
	return &config.City{
		Workspace:     config.Workspace{Name: "test-city", SessionTemplate: "{{.City}}--{{.Agent}}"},
		NamedSessions: []config.NamedSession{{Template: "named-worker", Mode: "on_demand"}},
	}
}

// useClosedNamedIndexClock pins the cache clock and empties the cache for one
// test, restoring both afterwards.
func useClosedNamedIndexClock(t *testing.T, now *time.Time) {
	t.Helper()
	prevNow := closedNamedIndexNow
	closedNamedIndexNow = func() time.Time { return *now }
	closedNamedIndexCache.reset()
	t.Cleanup(func() {
		closedNamedIndexNow = prevNow
		closedNamedIndexCache.reset()
	})
}

func closePhantomNamedSession(t *testing.T, store beads.Store, identity string) {
	t.Helper()
	b, err := store.Create(beads.Bead{
		Title:    "closed phantom",
		Type:     session.BeadType,
		Labels:   []string{session.LabelSession},
		Metadata: map[string]string{session.NamedSessionIdentityMetadata: identity},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(b.ID); err != nil {
		t.Fatal(err)
	}
}

// TestClosedNamedIndexIsReusedWithinTTL pins gcw-7gn7c: the closed named
// session index lists every session bead (closed included) and the caching
// store cannot serve IncludeClosed lists, so rebuilding it on every
// desired-state pass put ~11 full scans/min of a ~12k-row table on Dolt.
// Within the TTL, a pass must reuse the index and issue no store.List for it.
func TestClosedNamedIndexIsReusedWithinTTL(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	useClosedNamedIndexClock(t, &now)
	cfg := onDemandNamedSessionCity()
	store := &listCallCountingStore{MemStore: beads.NewMemStore()}

	readyAssignedWorkAssignees(cfg, store, nil, nil, nil, "")
	first := store.listCalls
	if first == 0 {
		t.Fatal("first pass issued no store.List; the closed index was never built")
	}

	now = now.Add(closedNamedIndexTTL / 2)
	readyAssignedWorkAssignees(cfg, store, nil, nil, nil, "")
	if got := store.listCalls - first; got != 0 {
		t.Fatalf("second pass within TTL issued %d store.List calls; want 0 (cached index reused)", got)
	}
}

// TestClosedNamedIndexRebuildsAfterTTL bounds staleness: a phantom closed after
// the index was built is seen once the TTL elapses, so its runtime-name
// assignee form is enumerated at most one TTL late.
func TestClosedNamedIndexRebuildsAfterTTL(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	useClosedNamedIndexClock(t, &now)
	cfg := onDemandNamedSessionCity()
	identity := cfg.NamedSessions[0].QualifiedName()
	runtimeName := config.NamedSessionRuntimeName("test-city", cfg.Workspace, identity)
	store := &listCallCountingStore{MemStore: beads.NewMemStore()}

	if got := readyAssignedWorkAssignees(cfg, store, nil, nil, nil, ""); containsString(got, runtimeName) {
		t.Fatalf("runtime name %q enumerated before any phantom exists", runtimeName)
	}
	closePhantomNamedSession(t, store, identity)

	now = now.Add(closedNamedIndexTTL / 2)
	if got := readyAssignedWorkAssignees(cfg, store, nil, nil, nil, ""); containsString(got, runtimeName) {
		t.Fatalf("runtime name %q enumerated within TTL; want the cached (pre-phantom) index", runtimeName)
	}

	now = now.Add(closedNamedIndexTTL)
	if got := readyAssignedWorkAssignees(cfg, store, nil, nil, nil, ""); !containsString(got, runtimeName) {
		t.Fatalf("runtime name %q not enumerated after TTL; got %v (index must rebuild and see the new phantom)", runtimeName, got)
	}
}

// TestClosedNamedIndexDoesNotCacheFailedOrPartialBuilds keeps the fail-open
// contract cheap to recover from: a hard failure or a partial read is never
// cached, so the next pass retries the full build.
func TestClosedNamedIndexDoesNotCacheFailedOrPartialBuilds(t *testing.T) {
	for _, tc := range []struct {
		name string
		hard bool
	}{
		{"hard failure", true},
		{"partial read", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
			useClosedNamedIndexClock(t, &now)
			cfg := onDemandNamedSessionCity()
			inner := &closedIndexStore{MemStore: beads.NewMemStore(), hard: tc.hard}
			store := &closedIndexCountingStore{closedIndexStore: inner}

			readyAssignedWorkAssignees(cfg, store, nil, nil, nil, "")
			first := store.listCalls
			readyAssignedWorkAssignees(cfg, store, nil, nil, nil, "")
			if got := store.listCalls - first; got != first {
				t.Fatalf("second pass issued %d store.List calls after a %s; want %d (no caching of a degraded build)", got, tc.name, first)
			}
		})
	}
}

// TestClosedNamedIndexIsPerStore keeps cities apart: the supervisor runs
// several cities in one process, each with its own store.
func TestClosedNamedIndexIsPerStore(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	useClosedNamedIndexClock(t, &now)
	cfg := onDemandNamedSessionCity()
	identity := cfg.NamedSessions[0].QualifiedName()
	runtimeName := config.NamedSessionRuntimeName("test-city", cfg.Workspace, identity)

	empty := &listCallCountingStore{MemStore: beads.NewMemStore()}
	withPhantom := &listCallCountingStore{MemStore: beads.NewMemStore()}
	closePhantomNamedSession(t, withPhantom, identity)

	readyAssignedWorkAssignees(cfg, empty, nil, nil, nil, "")
	if got := readyAssignedWorkAssignees(cfg, withPhantom, nil, nil, nil, ""); !containsString(got, runtimeName) {
		t.Fatalf("store with a phantom did not enumerate %q; another store's cached index leaked: got %v", runtimeName, got)
	}
	if withPhantom.listCalls == 0 {
		t.Fatal("second store issued no store.List; it reused the first store's index")
	}
}

type closedIndexCountingStore struct {
	*closedIndexStore
	listCalls int
}

func (s *closedIndexCountingStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.listCalls++
	return s.closedIndexStore.List(query)
}
