package main

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
)

type controlReadyScanCountingStore struct {
	beads.Store
	reads  int
	closes int
	err    error
}

// Ready records each authoritative read and can inject a ledger failure.
func (s *controlReadyScanCountingStore) Ready(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	s.reads++
	if s.err != nil {
		return nil, s.err
	}
	return s.Store.Ready(query...)
}

// CloseStore records handle ownership without closing the shared MemStore.
func (s *controlReadyScanCountingStore) CloseStore() error { //nolint:unparam // implements the store-handle close contract
	s.closes++
	return nil
}

// Fallback tests model an unavailable leg explicitly; they must not provision
// a real backend just to reach the discovery path they own.
func unavailableControlReadyLiveSources(t *testing.T) {
	t.Helper()
	previous := controlReadyCacheSourcesFn
	controlReadyCacheSourcesFn = func(_, _ string, _ *config.City) ([]beads.Store, []beads.Store, error) {
		return nil, nil, errors.New("fixture: live store unavailable")
	}
	t.Cleanup(func() { controlReadyCacheSourcesFn = previous })
}

// A worker's shim environment must not turn one ledger scan into a failed
// whole-set summary followed by a query for every assignee and route. Read
// the authoritative leg once, including controls beyond the summary row cap,
// and observe writes on the very next scan (upstream #7404 and #7446).
func TestControlReadyShimmedScanReadsOneCompleteLiveLeg(t *testing.T) {
	dir := t.TempDir()
	mem := beads.NewMemStore()
	store := &controlReadyScanCountingStore{Store: mem}
	installControlReadyCacheSourcesFn(t, dir, func(_, _ string, _ *config.City) ([]beads.Store, []beads.Store, error) {
		return []beads.Store{store}, nil, nil
	})
	for range controlReadyFallbackLimit + 1 {
		if _, err := mem.Create(beads.Bead{Type: "task", Assignee: "unrelated-worker"}); err != nil {
			t.Fatal(err)
		}
	}
	target := "gascity/control-dispatcher"
	priority := 4
	control, err := mem.Create(beads.Bead{Type: "task", Priority: &priority, Metadata: map[string]string{
		beadmeta.RoutedToMetadataKey: target,
		beadmeta.KindMetadataKey:     "check",
	}})
	if err != nil {
		t.Fatal(err)
	}
	previousRunner := controlReadyCommandRunner
	t.Cleanup(func() { controlReadyCommandRunner = previousRunner })
	commands := 0
	controlReadyCommandRunner = func(string, []string, string, string, []string) (string, error) {
		commands++
		return "", errors.New("unexpected summary subprocess: live ledger was available")
	}
	query := workflowServeControlReadyQuery(config.Agent{Name: config.ControlDispatcherAgentName, Dir: "gascity"})
	env := map[string]string{"GC_BEADS": "bd", citylayout.RealBdEnvVar: "/opt/bd"}
	scan := func() []hookBead {
		t.Helper()
		queue, handled, err := tryControlReadyFromCacheOrFallback(query, dir, env)
		if err != nil || !handled {
			t.Fatalf("scan: handled=%t err=%v", handled, err)
		}
		return queue
	}
	if got := scan(); len(got) != 1 || got[0].ID != control.ID {
		t.Fatalf("queue = %v; want control %s beyond summary cap", got, control.ID)
	}
	if err := mem.Close(control.ID); err != nil {
		t.Fatal(err)
	}
	if got := scan(); len(got) != 0 {
		t.Fatalf("next scan retained closed control: %v", got)
	}
	if store.reads != 2 || commands != 0 {
		t.Fatalf("two scans: Ready calls=%d subprocesses=%d; want 2, 0", store.reads, commands)
	}
}

func TestControlReadyLiveLegsCloseOwnedHandlesAndRejectPartialReads(t *testing.T) {
	for _, failure := range []int{-1, 0, 1} {
		t.Run(fmt.Sprintf("failure=%d", failure), func(t *testing.T) {
			dir := t.TempDir()
			first := &controlReadyScanCountingStore{Store: beads.NewMemStore()}
			second := &controlReadyScanCountingStore{Store: beads.NewMemStore()}
			sources := []*controlReadyScanCountingStore{first, second}
			readErr := errors.New("ledger unreadable")
			if failure >= 0 {
				sources[failure].err = readErr
			}
			installControlReadyCacheSourcesFn(t, dir, func(_, _ string, _ *config.City) ([]beads.Store, []beads.Store, error) {
				// The second source represents a shared graph binding.
				return []beads.Store{first, second}, []beads.Store{first}, nil
			})
			rows, opened, err := controlReadyLiveLegsReady(dir, dir, nil)
			if !opened {
				t.Fatal("opened legs were reported unavailable")
			}
			if failure >= 0 {
				if !errors.Is(err, readErr) || rows != nil {
					t.Fatalf("failed leg: rows=%v err=%v; want complete failure", rows, err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if first.closes != 1 || second.closes != 0 {
				t.Fatalf("closed owned=%d shared=%d; want 1, 0", first.closes, second.closes)
			}
			wantSecondReads := 1
			if failure == 0 {
				wantSecondReads = 0
			}
			if first.reads != 1 || second.reads != wantSecondReads {
				t.Fatalf("leg reads=%d,%d; want 1,%d", first.reads, second.reads, wantSecondReads)
			}
		})
	}
}

func TestControlReadyLiveReadFailureCannotUseStaleSummary(t *testing.T) {
	previousNow := controlReadyNow
	controlReadyNow = func() time.Time { return time.Unix(1700000000, 0) }
	t.Cleanup(func() { controlReadyNow = previousNow })
	dir := t.TempDir()
	readErr := errors.New("live read failed")
	store := &controlReadyScanCountingStore{Store: beads.NewMemStore(), err: readErr}
	installControlReadyCacheSourcesFn(t, dir, func(_, _ string, _ *config.City) ([]beads.Store, []beads.Store, error) {
		return []beads.Store{store}, []beads.Store{store}, nil
	})
	query := workflowServeControlReadyQuery(config.Agent{Name: config.ControlDispatcherAgentName, Dir: "gascity"})
	parsed, ok := parseControlReadyQuery(query)
	if !ok {
		t.Fatal("fixture query is not a control query")
	}
	env := map[string]string{"GC_BEADS": "bd", citylayout.RealBdEnvVar: "/opt/bd"}
	cached := &controlReadyCacheEntry{
		ready:            []beads.Bead{{ID: "stale-control", Assignee: parsed.target}},
		primedAt:         controlReadyNow(),
		includeEphemeral: parsed.includeEphemeral,
		queryKey:         controlReadyScopedQueryKey(parsed, mergeRuntimeEnv(os.Environ(), env)),
	}
	controlReadyCacheRegistry.mu.Lock()
	controlReadyCacheRegistry.byDir[dir] = cached
	controlReadyCacheRegistry.mu.Unlock()
	if got := controlReadyCacheFor(dir, dir, nil, env, parsed.includeEphemeral, parsed); got != cached {
		t.Fatal("fixture did not establish an eligible stale fallback snapshot")
	}
	if len(evaluateControlReady(cached.ready, parsed, mergeRuntimeEnv(os.Environ(), env))) != 1 {
		t.Fatal("fixture snapshot would not offer the stale control")
	}
	queue, handled, err := tryControlReadyFromCacheOrFallback(query, dir, env)
	if !handled || !errors.Is(err, readErr) || queue != nil {
		t.Fatalf("scan: queue=%v handled=%t err=%v; want live error, not stale success", queue, handled, err)
	}
	if store.closes != 1 {
		t.Fatalf("owned closes=%d; want 1 on read failure", store.closes)
	}
}

func TestControlReadyLiveLegBypassesPrimedCache(t *testing.T) {
	dir := t.TempDir()
	mem := beads.NewMemStore()
	control, err := mem.Create(beads.Bead{Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	cache := beads.NewCachingStore(mem, nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatal(err)
	}
	if err := mem.Close(control.ID); err != nil {
		t.Fatal(err)
	}
	stale, ok := cache.CachedReady()
	if !ok || len(stale) != 1 || stale[0].ID != control.ID {
		t.Fatalf("fixture did not preserve a stale snapshot: ready=%v ok=%t", stale, ok)
	}
	installControlReadyCacheSourcesFn(t, dir, func(_, _ string, _ *config.City) ([]beads.Store, []beads.Store, error) {
		return []beads.Store{cache}, nil, nil
	})
	rows, opened, err := controlReadyLiveLegsReady(dir, dir, nil)
	if err != nil || !opened || len(rows) != 0 {
		t.Fatalf("live read through cache wrapper: rows=%v opened=%t err=%v", rows, opened, err)
	}
}

func TestControlReadyUnavailableLiveLegUsesBoundedFallback(t *testing.T) {
	for _, openErr := range []bool{false, true} {
		t.Run(fmt.Sprintf("open_error=%t", openErr), func(t *testing.T) {
			dir := t.TempDir()
			installControlReadyCacheSourcesFn(t, dir, func(_, _ string, _ *config.City) ([]beads.Store, []beads.Store, error) {
				if openErr {
					return nil, nil, errors.New("fixture: unavailable")
				}
				return nil, nil, nil
			})
			previousRunner := controlReadyCommandRunner
			t.Cleanup(func() { controlReadyCommandRunner = previousRunner })
			commands := 0
			controlReadyCommandRunner = func(_ string, args []string, _, _ string, _ []string) (string, error) {
				commands++
				if !slices.Contains(args, "--summary-json") {
					t.Fatalf("fallback is not bounded: %v", args)
				}
				return `{"schema_version":"1","kind":"gc.bead_summary","verb":"ready","total":0,"omitted":0,"beads":[]}`, nil
			}
			query := workflowServeControlReadyQuery(config.Agent{Name: config.ControlDispatcherAgentName, Dir: "gascity"})
			queue, handled, err := tryControlReadyFromCacheOrFallback(query, dir, map[string]string{"GC_BEADS": "bd", citylayout.RealBdEnvVar: "/opt/bd"})
			if err != nil || !handled || len(queue) != 0 || commands != 1 {
				t.Fatalf("fallback: queue=%v handled=%t err=%v commands=%d; want one complete bounded read", queue, handled, err, commands)
			}
		})
	}
}

func TestControlReadyLiveBdLegPreservesBlockedWorkOutcome(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		t.Run(fmt.Sprintf("blocked=%t", blocked), func(t *testing.T) {
			dir := t.TempDir()
			reads, blockerLookups := 0, 0
			store := beads.NewBdStore(dir, func(_, _ string, args ...string) ([]byte, error) {
				switch args[0] {
				case "ready":
					reads++
					return []byte(`[{"id":"bd-dependent","status":"open","issue_type":"task","dependency_count":1,"dependencies":[{"issue_id":"bd-dependent","depends_on_id":"bd-blocker","type":"blocks"}]}]`), nil
				case "list":
					blockerLookups++
					metadata := ""
					if blocked {
						metadata = `,"metadata":{"gc.work_outcome":"blocked"}`
					}
					return []byte(`[{"id":"bd-blocker","status":"closed","issue_type":"task"` + metadata + `}]`), nil
				case "query":
					return []byte(`[]`), nil
				default:
					return nil, fmt.Errorf("unexpected bd command: %v", args)
				}
			})
			installControlReadyCacheSourcesFn(t, dir, func(_, _ string, _ *config.City) ([]beads.Store, []beads.Store, error) {
				return []beads.Store{store}, nil, nil
			})
			rows, opened, err := controlReadyLiveLegsReady(dir, dir, nil)
			if err != nil || !opened {
				t.Fatalf("live read: opened=%t err=%v", opened, err)
			}
			want := 1
			if blocked {
				want = 0
			}
			if len(rows) != want || reads != 1 || blockerLookups == 0 {
				t.Fatalf("rows=%v ready calls=%d blocker lookups=%d; want %d rows and a witnessed veto", rows, reads, blockerLookups, want)
			}
		})
	}
}
