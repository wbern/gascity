package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// Each attempted dry-run arm must expose its own boundaries, not sweep timing.
func TestOrphanInvocationCompletionLog(t *testing.T) {
	withReapOrphansEnforced(t, false)
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })
	gc := newWispGC(15*time.Minute, time.Hour, 0).(*memoryWispGC)
	_, err := gc.runGCContext(context.Background(), beads.GraphStore{Store: beads.NewMemStore()}, beads.SessionStore{}, beads.MailStore{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(logs.String(), "wisp gc: orphan invocation completed start="); got != 1 {
		t.Fatalf("completion markers = %d, want one actual orphan arm boundary: %s", got, logs.String())
	}
}

// This store distinguishes the expensive orphan enumeration/proofs from the
// other arms. No candidates appear in the root scans, so every Get/Children/
// DepList here belongs to the orphan arm.
type orphanCountingStore struct {
	beads.Store
	orphanLists, otherLists, mailLists, proofs, deletes atomic.Int64
	onOrphan                                            func()
	beforeList                                          func(beads.ListQuery)
	orphanErr                                           error
	cancelBeforeOrphan                                  context.CancelFunc
}

func (s *orphanCountingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if s.beforeList != nil {
		s.beforeList(q)
	}
	if q.Status == "closed" && q.Type == "" && q.TierMode == beads.TierWisps {
		s.orphanLists.Add(1)
		if s.onOrphan != nil {
			s.onOrphan()
		}
		if s.orphanErr != nil {
			return nil, s.orphanErr
		}
		return []beads.Bead{
			{ID: "orphan-leaf", Type: "task", Status: "closed", CreatedAt: time.Unix(1, 0)},
			{ID: "orphan-member", Type: "task", Status: "closed", CreatedAt: time.Unix(1, 0), Metadata: map[string]string{"gc.root_bead_id": "gone-root"}},
		}, nil
	}
	s.otherLists.Add(1)
	if q.Type == "message" {
		s.mailLists.Add(1)
	}
	if s.cancelBeforeOrphan != nil {
		s.cancelBeforeOrphan()
	}
	return nil, nil
}

func (s *orphanCountingStore) Get(id string) (beads.Bead, error) {
	s.proofs.Add(1)
	return s.Store.Get(id)
}

func (s *orphanCountingStore) Children(id string, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	s.proofs.Add(1)
	return s.Store.Children(id, opts...)
}

func (s *orphanCountingStore) DepList(id, direction string) ([]beads.Dep, error) {
	s.proofs.Add(1)
	return s.Store.DepList(id, direction)
}
func (s *orphanCountingStore) Delete(id string) error { s.deletes.Add(1); return s.Store.Delete(id) }

type orphanTestClock struct{ nanos atomic.Int64 }

func (c *orphanTestClock) now() time.Time          { return time.Unix(0, c.nanos.Load()).UTC() }
func (c *orphanTestClock) advance(d time.Duration) { c.nanos.Add(int64(d)) }

func orphanLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

func orphanRuntimeFixture(t *testing.T) (*CityRuntime, *orphanCountingStore, *orphanTestClock) {
	t.Helper()
	clearInheritedBeadsEnv(t)
	stubReloadBeadsLifecycle(t, func(string, string, *config.City, io.Writer) error { return nil })
	withReapOrphansEnforced(t, false)
	city := t.TempDir()
	tomlPath := filepath.Join(city, "city.toml")
	writeOrphanConfig(t, tomlPath, "bd", "15m", "")
	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatal(err)
	}
	s := &orphanCountingStore{Store: beads.NewMemStore()}
	previous := openStoreFactoryForCity
	openStoreFactoryForCity = func(_ context.Context, opts beads.StoreOpenOptions) (beads.StoreOpenResult, error) {
		if opts.ScopeRoot != city || opts.Provider != "bd" {
			t.Errorf("actual open inputs = %q/%q, want %q/bd", opts.ScopeRoot, opts.Provider, city)
		}
		return beads.StoreOpenResult{Store: s}, nil
	}
	t.Cleanup(func() { openStoreFactoryForCity = previous })
	sp := runtime.NewFake()
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath: city, CityName: "test-city", TomlPath: tomlPath, Cfg: cfg, SP: sp, Dops: newDrainOps(sp), Rec: events.Discard, Stdout: io.Discard, Stderr: io.Discard,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
	})
	opened, err := openCityStoreResultAt(city)
	if err != nil {
		t.Fatal(err)
	}
	want := beads.StoreIdentity{CityPath: city, ScopeRoot: city, Provider: "bd"}
	if opened.Identity != want {
		t.Fatalf("target-like successful-open identity = %+v, want %+v", opened.Identity, want)
	}
	cr.setStandaloneStores(opened, nil)
	clock := &orphanTestClock{}
	clock.nanos.Store(time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC).UnixNano())
	cr.orphanNow = clock.now
	return cr, s, clock
}

func writeOrphanConfig(t *testing.T, path, provider, interval, extra string) {
	t.Helper()
	data := fmt.Sprintf("[workspace]\nname = \"test-city\"\n[session]\nprovider = \"fake\"\n[beads]\nprovider = %q\n[daemon]\nwisp_gc_interval = %q\nwisp_ttl = \"1h\"\n[mail]\nretention_ttl = \"1h\"\n%s", provider, interval, extra)
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func runOrphanSweep(t *testing.T, cr *CityRuntime, now time.Time) wispGCResult {
	t.Helper()
	cr.launchWispGC(now)
	if !cr.wispSweeps.waitIdle(hangBudget) {
		t.Fatal("sweep did not finish")
	}
	result, ok := cr.wispSweeps.harvest()
	if !ok {
		t.Fatal("sweep did not execute")
	}
	return result
}

func reloadOrphanRuntime(t *testing.T, cr *CityRuntime) {
	t.Helper()
	// Change the source revision so this reaches the real reload path rather
	// than the unchanged-revision fast path.
	data, err := os.ReadFile(cr.tomlPath)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, []byte(fmt.Sprintf("\n# generation after %s\n", cr.configRev))...)
	if err := os.WriteFile(cr.tomlPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	old := cr.wg
	provider := "fake"
	reply := cr.reloadConfigTraced(context.Background(), &provider, cr.cityPath, nil, reloadSourceManual)
	if reply.Outcome == reloadOutcomeFailed {
		t.Fatalf("reload failed: %s", reply.Error)
	}
	if cr.wg == old {
		t.Fatalf("reload did not replace tracker: %+v", reply)
	}
}

func TestOrphanCooldownLongArmReloadAndBoundary(t *testing.T) {
	logs := orphanLogs(t)
	cr, s, clock := orphanRuntimeFixture(t)
	start := clock.now()
	s.onOrphan = func() { clock.advance(22 * time.Minute) }
	result := runOrphanSweep(t, cr, start)
	if result.err != nil || result.purged != 0 || s.deletes.Load() != 0 {
		t.Fatalf("dry-run result=%+v deletes=%d", result, s.deletes.Load())
	}
	if s.orphanLists.Load() != 1 || s.proofs.Load() == 0 {
		t.Fatalf("initial orphan arm did not execute proofs: %+v", s)
	}
	s.onOrphan = nil
	// Without any reload, the next eligible sweep starts at minute 22. It must
	// skip the expensive arm while both preceding and following arms still run.
	beforeOther, beforeMail := s.otherLists.Load(), s.mailLists.Load()
	runOrphanSweep(t, cr, clock.now())
	if s.orphanLists.Load() != 1 || s.otherLists.Load() <= beforeOther || s.mailLists.Load() <= beforeMail {
		t.Fatal("normal later sweep did not isolate the cooldown to orphans")
	}
	cr.wg.(*memoryWispGC).sessionPurgeCursor = &beads.SeekBoundary{}
	proofs, other, mails := s.proofs.Load(), s.otherLists.Load(), s.mailLists.Load()
	for _, advance := range []time.Duration{0, 5 * time.Minute, 10*time.Minute - time.Nanosecond} {
		clock.advance(advance)
		reloadOrphanRuntime(t, cr) // real reload resets sweep cadence, not cooldown
		if m := cr.wg.(*memoryWispGC); !m.lastRun.IsZero() || m.sessionPurgeCursor != nil {
			t.Fatal("reload carried general cadence/cursor")
		}
		if got := runOrphanSweep(t, cr, clock.now()); got.err != nil || got.purged != 0 {
			t.Fatalf("skipped sweep: %+v", got)
		}
		if s.orphanLists.Load() != 1 || s.proofs.Load() != proofs {
			t.Fatalf("cooldown read orphan rows/proofs: lists=%d proofs=%d", s.orphanLists.Load(), s.proofs.Load())
		}
		if s.otherLists.Load() <= other {
			t.Fatal("other arms did not positively execute")
		}
		if s.mailLists.Load() <= mails {
			t.Fatal("later mail arm did not execute during cooldown")
		}
		mails = s.mailLists.Load()
		other = s.otherLists.Load()
	}
	if !strings.Contains(logs.String(), "completion cooldown remaining=1ns") || strings.Contains(logs.String(), "unknown graph store identity") {
		t.Fatalf("target did not positively take cooldown: %s", logs.String())
	}
	clock.advance(time.Nanosecond)
	reloadOrphanRuntime(t, cr)
	runOrphanSweep(t, cr, clock.now())
	if s.orphanLists.Load() != 2 {
		t.Fatalf("exact completion+15m boundary did not admit: %d", s.orphanLists.Load())
	}
	want := "start=" + start.Format(time.RFC3339Nano) + " end=" + start.Add(22*time.Minute).Format(time.RFC3339Nano)
	if !strings.Contains(logs.String(), want) {
		t.Fatalf("untruthful orphan boundaries, want %s: %s", want, logs.String())
	}
	if got := strings.Count(logs.String(), "orphan invocation completed"); got != 2 {
		t.Fatalf("completion count=%d want 2", got)
	}
	t.Log(logs.String())
}

func TestOrphanCooldownFailureCancelFallbackAndEnforcement(t *testing.T) {
	for _, scenario := range []string{"failure", "cancel", "unknown", "relocated", "enforced", "fast"} {
		t.Run(scenario, func(t *testing.T) {
			logs := orphanLogs(t)
			cr, s, clock := orphanRuntimeFixture(t)
			sentinel := errors.New("orphan read failed")
			ctx := context.Background()
			switch scenario {
			case "failure":
				s.orphanErr = sentinel
			case "cancel":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				defer cancel()
				s.cancelBeforeOrphan = cancel
			case "unknown":
				cr.standaloneStoreIdentity = beads.StoreIdentity{}
			case "relocated":
				cr.storageRoutes = &storageRoutes{stores: map[coordclass.Class]beads.Store{coordclass.ClassGraph: s}}
			case "enforced":
				withReapOrphansEnforced(t, true)
			}
			// This is the same production tracker/registry wiring as launch, with a
			// cancellable context so the earlier arm can cancel before orphan entry.
			m := cr.wg.(*memoryWispGC)
			graph, key := cr.orphanGraphStore()
			m.orphanCooldown = &cr.orphanCooldown
			m.orphanIdentity = key
			m.orphanNow = clock.now
			_, err := m.runGCContext(ctx, graph, beads.SessionStore{}, beads.MailStore{}, clock.now())
			if scenario == "failure" && !errors.Is(err, sentinel) {
				t.Fatalf("original error lost: %v", err)
			}
			if scenario == "cancel" {
				if !errors.Is(err, context.Canceled) || s.orphanLists.Load() != 0 || strings.Contains(logs.String(), "invocation completed") {
					t.Fatalf("pre-arm cancellation recorded attempt: err=%v logs=%s", err, logs.String())
				}
				s.cancelBeforeOrphan = nil
			}
			before := s.orphanLists.Load()
			clock.advance(time.Minute)
			_, _ = m.runGCContext(context.Background(), graph, beads.SessionStore{}, beads.MailStore{}, clock.now())
			skipped := scenario == "failure" || scenario == "fast"
			want := before + 1
			if skipped {
				want = before
			}
			if s.orphanLists.Load() != want {
				t.Fatalf("second arm count=%d want %d", s.orphanLists.Load(), want)
			}
			if (scenario == "unknown" || scenario == "relocated") && !strings.Contains(logs.String(), "unknown graph store identity; running original") {
				t.Fatal("fallback reason absent")
			}
			if scenario == "enforced" && strings.Contains(logs.String(), "invocation completed") {
				t.Fatal("enforced arm reported dry-run completion")
			}
			if scenario != "enforced" && s.deletes.Load() != 0 {
				t.Fatal("dry-run deleted rows")
			}
			markers := strings.Count(logs.String(), "orphan invocation completed")
			wantMarkers := 1
			if scenario == "unknown" || scenario == "relocated" {
				wantMarkers = 2
			}
			if scenario == "enforced" {
				wantMarkers = 0
			}
			if markers != wantMarkers {
				t.Fatalf("completion markers=%d want %d", markers, wantMarkers)
			}
			t.Logf("first result error=%v\n%s", err, logs.String())
		})
	}
}

func TestOrphanCooldownInFlightReloadCapturesIntervalAndRefusesStorageChange(t *testing.T) {
	logs := orphanLogs(t)
	cr, s, clock := orphanRuntimeFixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	s.onOrphan = func() { close(started); <-release; clock.advance(22 * time.Minute) }
	cr.launchWispGC(clock.now())
	awaitClose(t, started, "orphan arm entry")
	old := cr.wg
	writeOrphanConfig(t, cr.tomlPath, "bd", "3m", "")
	reloadOrphanRuntime(t, cr)
	if old == cr.wg || cr.wg.(*memoryWispGC).interval != 3*time.Minute {
		t.Fatal("actual reload did not replace tracker/interval")
	}
	// A real storage-changing reload must fail while A is still in flight.
	extra := fmt.Sprintf("[storage.classes]\ngraph = \"infra\"\n[storage.bindings.infra]\nprovider = %q\npath = %q\n", config.StorageProviderSQLiteBeads, filepath.Join(cr.cityPath, "relocated"))
	writeOrphanConfig(t, cr.tomlPath, "bd", "3m", extra)
	provider := "fake"
	reply := cr.reloadConfigTraced(context.Background(), &provider, cr.cityPath, nil, reloadSourceManual)
	if reply.Outcome != reloadOutcomeFailed || !strings.Contains(reply.Error, "[storage]") {
		t.Fatalf("storage change was not refused: %+v", reply)
	}
	writeOrphanConfig(t, cr.tomlPath, "bd", "3m", "")
	reloadOrphanRuntime(t, cr)
	close(release)
	if !cr.wispSweeps.waitIdle(hangBudget) {
		t.Fatal("old tracker did not finish")
	}
	_, _ = cr.wispSweeps.harvest()
	s.onOrphan = nil
	firstProofs := s.proofs.Load()
	for _, advance := range []time.Duration{0, 3 * time.Minute, 3 * time.Minute} {
		clock.advance(advance)
		reloadOrphanRuntime(t, cr)
		runOrphanSweep(t, cr, clock.now())
		if s.orphanLists.Load() != 1 || s.proofs.Load() != firstProofs {
			t.Fatal("reload/in-flight interval change shortened captured cooldown")
		}
	}
	if !strings.Contains(logs.String(), "remaining=15m0s") {
		t.Fatalf("gap did not use old invocation interval: %s", logs.String())
	}
	clock.advance(9 * time.Minute)
	runOrphanSweep(t, cr, clock.now())
	if s.orphanLists.Load() != 2 {
		t.Fatal("captured interval boundary did not admit")
	}
	t.Log(logs.String())
}

func TestOrphanCooldownCapturedRouteAndIndependentCities(t *testing.T) {
	cr, s, clock := orphanRuntimeFixture(t)
	a := cr.standaloneStoreResult()
	b := a
	b.Identity.Provider = "file"
	started, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	s.onOrphan = func() { close(started); <-release }
	cr.launchWispGC(clock.now())
	awaitClose(t, started, "orphan arm entry")
	cr.setStandaloneStores(b, nil)
	close(release)
	if !cr.wispSweeps.waitIdle(hangBudget) {
		t.Fatal("sweep did not complete")
	}
	_, _ = cr.wispSweeps.harvest()
	s.onOrphan = nil
	if cr.orphanCooldown.remaining(b.Identity, clock.now()) > 0 {
		t.Fatal("A completion updated selected B")
	}
	if cr.orphanCooldown.remaining(a.Identity, clock.now()) != 15*time.Minute {
		t.Fatal("A completion lost after B selection")
	}
	// The general sweep cadence resets exactly as on reload; independent B runs.
	cr.wg = newWispGCForConfig(cr.cfg)
	runOrphanSweep(t, cr, clock.now())
	if s.orphanLists.Load() != 2 {
		t.Fatal("B did not independently execute")
	}
	// Returning to A through a new wrapper must retain A's completed rest.
	a.Store = &orphanCountingStore{Store: beads.NewMemStore()}
	cr.setStandaloneStores(a, nil)
	cr.wg = newWispGCForConfig(cr.cfg)
	runOrphanSweep(t, cr, clock.now())
	if a.Store.(*orphanCountingStore).orphanLists.Load() != 0 {
		t.Fatal("A-to-B-to-A lost A cooldown")
	}
	otherCity := a
	otherCity.Identity.CityPath = filepath.Join(cr.cityPath, "other")
	otherCity.Identity.ScopeRoot = otherCity.Identity.CityPath
	cr.setStandaloneStores(otherCity, nil)
	cr.wg = newWispGCForConfig(cr.cfg)
	runOrphanSweep(t, cr, clock.now())
	if a.Store.(*orphanCountingStore).orphanLists.Load() != 1 {
		t.Fatal("independent city inherited another city cooldown")
	}
	fresh := &CityRuntime{cityPath: cr.cityPath, cfg: cr.cfg, wg: newWispGCForConfig(cr.cfg), orphanNow: clock.now, stdout: io.Discard, stderr: io.Discard}
	fresh.setStandaloneStores(a, nil)
	runOrphanSweep(t, fresh, clock.now())
	if a.Store.(*orphanCountingStore).orphanLists.Load() != 2 {
		t.Fatal("new runtime inherited process-global cooldown")
	}
}

func TestOrphanCooldownSuccessfulOpenAndReopenIdentity(t *testing.T) {
	logs := orphanLogs(t)
	cr, _, clock := orphanRuntimeFixture(t)
	// Exercise the actual controller constructor and update publication seam.
	// Wrappers may change; the captured successful-open identity must not.
	cacheCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cs := newControllerState(cacheCtx, cr.cfg, cr.sp, events.NewFake(), cr.cityName, cr.cityPath)
	cr.cs = cs
	_, a := cr.orphanGraphStore()
	if a.Provider != "bd" || a.ScopeRoot != cr.cityPath || a.CityPath != cr.cityPath {
		t.Fatalf("boot identity=%+v", a)
	}
	cr.orphanCooldown.complete(a, clock.now(), 15*time.Minute)
	before := cs.CityBeadStore()
	cs.update(cr.cfg, cr.sp)
	graph, key := cr.orphanGraphStore()
	if graph.Store == before || key != a {
		t.Fatalf("same-provider reopen: handle replaced=%v identity=%+v", graph.Store != before, key)
	}
	runOrphanSweep(t, cr, clock.now())
	if !strings.Contains(logs.String(), "completion cooldown remaining=15m0s") {
		t.Fatal("reopened target did not skip")
	}
	// Failed open retains the exact old pair, even with changed provider config.
	before = graph.Store
	failure := errors.New("reopen failed")
	openStoreFactoryForCity = func(context.Context, beads.StoreOpenOptions) (beads.StoreOpenResult, error) {
		return beads.StoreOpenResult{}, failure
	}
	writeOrphanConfig(t, cr.tomlPath, "file", "15m", "")
	next, err := config.Load(osFS{}, cr.tomlPath)
	if err != nil {
		t.Fatal(err)
	}
	cs.update(next, cr.sp)
	graph, key = cr.orphanGraphStore()
	if graph.Store != before || key != a {
		t.Fatal("failed reopen replaced handle or identity")
	}
	openStoreFactoryForCity = func(_ context.Context, opts beads.StoreOpenOptions) (beads.StoreOpenResult, error) {
		if opts.Provider != "file" {
			t.Fatalf("new provider not resolved at open: %q", opts.Provider)
		}
		return beads.StoreOpenResult{Store: beads.NewMemStore()}, nil
	}
	cs.update(next, cr.sp)
	graph, key = cr.orphanGraphStore()
	if graph.Store == before || key.Provider != "file" || cr.orphanCooldown.remaining(key, clock.now()) > 0 {
		t.Fatalf("changed-provider reopen did not have independent key: %+v", key)
	}
	// Unknown providers do not inherit a fabricated identity from config spelling.
	writeOrphanConfig(t, cr.tomlPath, "exec:custom", "15m", "")
	openStoreFactoryForCity = func(context.Context, beads.StoreOpenOptions) (beads.StoreOpenResult, error) {
		return beads.StoreOpenResult{Store: beads.NewMemStore()}, nil
	}
	opened, err := openCityStoreResultAt(cr.cityPath)
	if err != nil {
		t.Fatal(err)
	}
	if opened.Identity != (beads.StoreIdentity{}) {
		t.Fatalf("custom provider identity manufactured: %+v", opened.Identity)
	}
}

func TestOrphanCooldownStandaloneFailedReopenRetainsPair(t *testing.T) {
	logs := orphanLogs(t)
	cr, _, clock := orphanRuntimeFixture(t)
	runOrphanSweep(t, cr, clock.now())
	before, key := cr.orphanGraphStore()
	openStoreFactoryForCity = func(context.Context, beads.StoreOpenOptions) (beads.StoreOpenResult, error) {
		return beads.StoreOpenResult{}, errors.New("unavailable")
	}
	reloadOrphanRuntime(t, cr)
	after, got := cr.orphanGraphStore()
	if after.Store != before.Store || got != key {
		t.Fatalf("failed standalone reopen replaced captured pair: %+v", got)
	}
	clock.advance(time.Minute)
	runOrphanSweep(t, cr, clock.now())
	if !strings.Contains(logs.String(), "completion cooldown remaining=14m0s") {
		t.Fatalf("failed reopen discarded cooldown: %s", logs.String())
	}
}

func TestOrphanCooldownFastArmSkipsNextScheduledSweep(t *testing.T) {
	logs := orphanLogs(t)
	cr, s, clock := orphanRuntimeFixture(t)
	start := clock.now()
	s.onOrphan = func() { clock.advance(time.Minute) }
	runOrphanSweep(t, cr, start)
	s.onOrphan = nil
	clock.advance(14 * time.Minute)
	runOrphanSweep(t, cr, clock.now())
	if s.orphanLists.Load() != 1 || !strings.Contains(logs.String(), "completion cooldown remaining=1m0s") {
		t.Fatalf("fast arm did not rest after completion: lists=%d logs=%s", s.orphanLists.Load(), logs.String())
	}
}

func TestOrphanInvocationBoundariesExcludeOtherArms(t *testing.T) {
	logs := orphanLogs(t)
	cr, s, clock := orphanRuntimeFixture(t)
	start := clock.now()
	var earlier atomic.Bool
	s.beforeList = func(q beads.ListQuery) {
		if !earlier.Swap(true) {
			clock.advance(2 * time.Minute)
		}
		if q.Type == "message" {
			clock.advance(3 * time.Minute)
		}
	}
	s.onOrphan = func() { clock.advance(22 * time.Minute) }
	runOrphanSweep(t, cr, start)
	want := "start=" + start.Add(2*time.Minute).Format(time.RFC3339Nano) + " end=" + start.Add(24*time.Minute).Format(time.RFC3339Nano)
	if !strings.Contains(logs.String(), want) || clock.now() != start.Add(27*time.Minute) {
		t.Fatalf("marker included time from other arms, want %s: %s", want, logs.String())
	}
	_, key := cr.orphanGraphStore()
	if remaining := cr.orphanCooldown.remaining(key, clock.now()); remaining != 12*time.Minute {
		t.Fatalf("cooldown anchored to sweep footer: %s", remaining)
	}
	t.Log(logs.String())
}
