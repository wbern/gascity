package main

import (
	"bytes"
	"database/sql"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
)

const idleGateTarget = "gascity/control-dispatcher"

// idleGateStore counts authoritative ready reads, can fail them, and can run
// a hook after the frontier is read (a write racing the scan).
type idleGateStore struct {
	beads.Store
	reads   int
	err     error
	onReady func()
}

// Ready records each read; onReady runs after the frontier was taken.
func (s *idleGateStore) Ready(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	s.reads++
	if s.err != nil {
		return nil, s.err
	}
	rows, err := s.Store.Ready(query...)
	if s.onReady != nil {
		s.onReady()
	}
	return rows, err
}

// idleGateFixture wires one counting ledger, a settable store version and a
// settable clock behind the production serve-loop read (nextWorkflowServeBeads).
// store.reads counts Ready calls: on a bd scope each one is a forked
// `bd ready --json --include-ephemeral --limit 0`.
type idleGateFixture struct {
	t         *testing.T
	dir       string
	mem       *beads.MemStore
	store     *idleGateStore
	version   string
	versioned bool
	now       time.Time
	query     string
	legErr    error // non-nil: the scope's ledgers cannot be opened
}

func newIdleGateFixture(t *testing.T) *idleGateFixture {
	t.Helper()
	f := &idleGateFixture{
		t:         t,
		dir:       t.TempDir(),
		mem:       beads.NewMemStore(),
		version:   "v1",
		versioned: true,
		now:       time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC),
	}
	f.store = &idleGateStore{Store: f.mem}
	installControlReadyLegSourcesFn(t, func(_, _ string, _ *config.City) ([]beads.Store, []beads.Store, error) {
		if f.legErr != nil {
			return nil, nil, f.legErr
		}
		return []beads.Store{f.store}, nil, nil
	})
	prevVersion, prevNow := controlReadyStoreVersionFn, controlReadyNow
	controlReadyStoreVersionFn = func(string) (string, bool) { return f.version, f.versioned }
	controlReadyNow = func() time.Time { return f.now }
	t.Cleanup(func() {
		controlReadyStoreVersionFn, controlReadyNow = prevVersion, prevNow
		controlReadyIdleGate.mu.Lock()
		controlReadyIdleGate.byDir = make(map[string]controlReadyIdleEntry)
		controlReadyIdleGate.mu.Unlock()
	})
	f.query = workflowServeControlReadyQuery(config.Agent{Name: config.ControlDispatcherAgentName, Dir: "gascity"})
	return f
}

// tick is one serve-loop read of this scope, through the production seam.
func (f *idleGateFixture) tick() []hookBead {
	f.t.Helper()
	queue, err := nextWorkflowServeBeads(f.query, f.dir, nil)
	if err != nil {
		f.t.Fatalf("nextWorkflowServeBeads: %v", err)
	}
	return queue
}

func (f *idleGateFixture) createControl() beads.Bead {
	f.t.Helper()
	b, err := f.mem.Create(beads.Bead{Type: "task", Metadata: map[string]string{
		beadmeta.RoutedToMetadataKey: idleGateTarget,
		beadmeta.KindMetadataKey:     "check",
	}})
	if err != nil {
		f.t.Fatal(err)
	}
	return b
}

func queueHas(queue []hookBead, id string) bool {
	for _, b := range queue {
		if b.ID == id {
			return true
		}
	}
	return false
}

// An idle scope whose database has not changed costs one in-process Ready call
// (on a bd scope, one forked `bd ready`) per rescan interval, not one per tick.
func TestControlReadyIdleGateIdleTicksMakeOneReadyCallPerRescanInterval(t *testing.T) {
	f := newIdleGateFixture(t)
	// The follow loop's steady idle cadence: one tick per workflowServeMaxIdleSleep.
	ticks := int(controlReadyIdleRescanInterval / workflowServeMaxIdleSleep)
	for i := 0; i < ticks; i++ {
		if got := f.tick(); len(got) != 0 {
			t.Fatalf("tick %d: queue = %v, want empty", i, got)
		}
		f.now = f.now.Add(workflowServeMaxIdleSleep)
	}
	if ticks < 2 || f.store.reads != 1 {
		t.Fatalf("Ready calls over %d idle ticks within one rescan interval = %d, want 1 (the priming scan)", ticks, f.store.reads)
	}
}

// A control bead that becomes ready changes the database, so the very next
// tick scans and returns it: the pickup latency is the unchanged tick cadence.
func TestControlReadyIdleGatePicksUpNewControlOnTheNextTick(t *testing.T) {
	f := newIdleGateFixture(t)
	f.tick()
	f.tick()
	if f.store.reads != 1 {
		t.Fatalf("Ready calls after an idle tick = %d, want 1", f.store.reads)
	}
	control := f.createControl()
	f.version = "v2" // the write that made it ready changes DOLT_HASHOF_DB()
	if got := f.tick(); !queueHas(got, control.ID) {
		t.Fatalf("tick after the write: queue = %v, want %s with no clock advance", got, control.ID)
	}
	if f.store.reads != 2 {
		t.Fatalf("Ready calls = %d, want 2", f.store.reads)
	}
}

// A write that lands while a scan is in flight must not be hidden: the gate
// records the version read BEFORE the scan, so the write's new version
// forces the next tick to scan.
func TestControlReadyIdleGateWriteRacingTheScanIsSeenNextTick(t *testing.T) {
	f := newIdleGateFixture(t)
	var control beads.Bead
	raced := false
	f.store.onReady = func() {
		if raced {
			return
		}
		raced = true
		// The scan has read its (empty) frontier; a write now commits.
		control = f.createControl()
		f.version = "v2"
	}
	if got := f.tick(); len(got) != 0 {
		t.Fatalf("racing scan: queue = %v, want the pre-write empty frontier", got)
	}
	if got := f.tick(); !queueHas(got, control.ID) {
		t.Fatalf("tick after the racing write: queue = %v, want %s", got, control.ID)
	}
}

// Readiness inputs the hash cannot see (a passing defer_until, a dependency in
// another database, a wrong-database probe) are still bounded: an unchanged
// scope is fully re-scanned once controlReadyIdleRescanInterval elapses.
func TestControlReadyIdleGateRescansAfterIntervalEvenWhenUnchanged(t *testing.T) {
	f := newIdleGateFixture(t)
	start := f.now
	f.tick()
	control := f.createControl() // invisible to the version: it stays "v1"
	f.now = start.Add(controlReadyIdleRescanInterval - time.Nanosecond)
	if got := f.tick(); len(got) != 0 {
		t.Fatalf("inside the interval: queue = %v, want the skipped empty answer", got)
	}
	f.now = start.Add(controlReadyIdleRescanInterval)
	if got := f.tick(); !queueHas(got, control.ID) {
		t.Fatalf("at the interval: queue = %v, want %s from a forced rescan", got, control.ID)
	}
	if f.store.reads != 2 {
		t.Fatalf("Ready calls = %d, want 2", f.store.reads)
	}
}

// Work in the queue -- processed, pending, skipped or quiet -- is never
// answered from the gate: only an empty scan is remembered.
func TestControlReadyIdleGateNeverSkipsWhileTheQueueHasWork(t *testing.T) {
	f := newIdleGateFixture(t)
	control := f.createControl()
	for i := 0; i < 3; i++ {
		if got := f.tick(); !queueHas(got, control.ID) {
			t.Fatalf("tick %d: queue = %v, want %s", i, got, control.ID)
		}
	}
	if f.store.reads != 3 {
		t.Fatalf("Ready calls = %d, want one per tick", f.store.reads)
	}
	// Once it is processed and the queue drains, idling resumes.
	if err := f.mem.Close(control.ID); err != nil {
		t.Fatal(err)
	}
	f.version = "v2"
	f.tick()
	f.tick()
	if f.store.reads != 4 {
		t.Fatalf("Ready calls after the queue drained = %d, want 4", f.store.reads)
	}
}

// A scope the gate cannot version scans every tick, exactly as before.
func TestControlReadyIdleGateUnversionedScopeScansEveryTick(t *testing.T) {
	f := newIdleGateFixture(t)
	f.versioned = false
	for i := 0; i < 3; i++ {
		f.tick()
	}
	if f.store.reads != 3 {
		t.Fatalf("Ready calls = %d, want 3", f.store.reads)
	}
}

// A losing version read drops the remembered scan: when the probe recovers
// with the same version, the gate still scans once before skipping again.
func TestControlReadyIdleGateProbeFailureForgetsTheRememberedScan(t *testing.T) {
	f := newIdleGateFixture(t)
	f.tick()
	f.versioned = false
	f.tick()
	f.versioned = true
	f.tick()
	f.tick()
	if f.store.reads != 3 {
		t.Fatalf("Ready calls = %d, want 3 (prime, unversioned, re-prime)", f.store.reads)
	}
}

// A failed scan answers nothing and must not be remembered as "empty".
func TestControlReadyIdleGateDoesNotRememberAFailedScan(t *testing.T) {
	f := newIdleGateFixture(t)
	f.store.err = errors.New("ledger unreadable")
	f.tick()
	f.store.err = nil
	control := f.createControl() // same version: only the failure hid it
	if got := f.tick(); !queueHas(got, control.ID) {
		t.Fatalf("tick after a failed scan: queue = %v, want %s", got, control.ID)
	}
}

// The remembered answer belongs to one dispatcher identity: another
// identity's query against the same scope scans for itself.
func TestControlReadyIdleGateIsKeyedByDispatcherIdentity(t *testing.T) {
	f := newIdleGateFixture(t)
	f.tick()
	other := workflowServeControlReadyQuery(config.Agent{Name: config.ControlDispatcherAgentName, Dir: "other-rig"})
	if _, err := nextWorkflowServeBeads(other, f.dir, nil); err != nil {
		t.Fatal(err)
	}
	if f.store.reads != 2 {
		t.Fatalf("Ready calls = %d, want 2 (one per identity)", f.store.reads)
	}
}

func TestControlReadyTargetVersionable(t *testing.T) {
	cases := []struct {
		name   string
		target contract.DoltConnectionTarget
		want   bool
	}{
		{"managed tcp", contract.DoltConnectionTarget{Port: "3307", Database: "hq"}, true},
		{"managed socket", contract.DoltConnectionTarget{Socket: "/run/dolt.sock", Database: "hq"}, true},
		{"external", contract.DoltConnectionTarget{Port: "3307", Database: "hq", External: true}, false},
		{"proxied", contract.DoltConnectionTarget{Database: "hq", DoltMode: "proxied-server"}, false},
		{"no endpoint", contract.DoltConnectionTarget{Database: "hq"}, false},
		{"no database", contract.DoltConnectionTarget{Port: "3307"}, false},
	}
	for _, tc := range cases {
		if got := controlReadyTargetVersionable(tc.target); got != tc.want {
			t.Errorf("%s: versionable = %t, want %t", tc.name, got, tc.want)
		}
	}
}

func TestControlReadyTargetMatchesEnv(t *testing.T) {
	tcp := contract.DoltConnectionTarget{Port: "3307", Database: "hq"}
	sock := contract.DoltConnectionTarget{Socket: "/run/dolt.sock", Database: "hq"}
	cases := []struct {
		name   string
		target contract.DoltConnectionTarget
		env    []string
		want   bool
	}{
		{"no env pins: bd discovers its own endpoint", tcp, nil, false},
		{"same port", tcp, []string{"GC_DOLT_PORT=3307", "BEADS_DOLT_SERVER_PORT=3307"}, true},
		{"same port, cleared mirror", tcp, []string{"GC_DOLT_PORT=3307", "BEADS_DOLT_SERVER_PORT="}, true},
		{"bd pointed at another port", tcp, []string{"GC_DOLT_PORT=4400"}, false},
		{"bd pointed at another server port", tcp, []string{"GC_DOLT_PORT=3307", "BEADS_DOLT_SERVER_PORT=4400"}, false},
		{"bd pointed at another database", tcp, []string{"GC_DOLT_PORT=3307", "BEADS_DOLT_SERVER_DATABASE=other"}, false},
		{"tcp target, bd on a socket", tcp, []string{"GC_DOLT_PORT=3307", "BEADS_DOLT_SERVER_SOCKET=/run/dolt.sock"}, false},
		{"same socket", sock, []string{"BEADS_DOLT_SERVER_SOCKET=/run/dolt.sock"}, true},
		{"socket target, no socket pinned", sock, nil, false},
		{"another socket", sock, []string{"BEADS_DOLT_SERVER_SOCKET=/tmp/x.sock"}, false},
	}
	for _, tc := range cases {
		if got := controlReadyTargetMatchesEnv(tc.target, tc.env); got != tc.want {
			t.Errorf("%s: matches = %t, want %t", tc.name, got, tc.want)
		}
	}
}

func TestControlReadyReadDoltVersionSurfacesOpenFailure(t *testing.T) {
	prev := controlReadyOpenVersionDB
	t.Cleanup(func() { controlReadyOpenVersionDB = prev })
	openErr := errors.New("dial refused")
	controlReadyOpenVersionDB = func(contract.DoltConnectionTarget) (*sql.DB, error) { return nil, openErr }
	if _, err := controlReadyReadDoltVersion(contract.DoltConnectionTarget{Port: "3307", Database: "hq"}); !errors.Is(err, openErr) {
		t.Fatalf("err = %v, want %v", err, openErr)
	}
}

// An empty answer served from the bounded ready cache, after the scope's
// ledgers failed to open, is not a live read: it must not be remembered, or a
// stale snapshot would be pinned to a fresh hash.
func TestControlReadyIdleGateDoesNotRememberACachedAnswer(t *testing.T) {
	f := newIdleGateFixture(t)
	env := map[string]string{citylayout.RealBdEnvVar: "/nonexistent/bd"}
	parsed, ok := parseControlReadyQuery(f.query)
	if !ok {
		t.Fatalf("query %q is not a control-ready query", f.query)
	}
	controlReadyCacheRegistry.mu.Lock()
	prevCache := controlReadyCacheRegistry.byDir
	controlReadyCacheRegistry.byDir = map[string]*controlReadyCacheEntry{f.dir: {
		queryKey:         controlReadyScopedQueryKey(parsed, mergeRuntimeEnv(os.Environ(), env)),
		primedAt:         f.now,
		includeEphemeral: parsed.includeEphemeral,
	}}
	controlReadyCacheRegistry.mu.Unlock()
	t.Cleanup(func() {
		controlReadyCacheRegistry.mu.Lock()
		controlReadyCacheRegistry.byDir = prevCache
		controlReadyCacheRegistry.mu.Unlock()
	})

	f.legErr = errors.New("ledger open failed")
	queue, err := nextWorkflowServeBeads(f.query, f.dir, env)
	if err != nil || len(queue) != 0 {
		t.Fatalf("cached tick: queue = %v, err = %v; want the cached empty answer", queue, err)
	}
	f.legErr = nil
	control := f.createControl() // same version: only the stale snapshot hid it
	queue, err = nextWorkflowServeBeads(f.query, f.dir, env)
	if err != nil || !queueHas(queue, control.ID) {
		t.Fatalf("tick after a cached answer: queue = %v, err = %v; want %s from a live scan", queue, err, control.ID)
	}
}

// When the scope's ledgers cannot be opened the scan shells `bd ready`. That
// fallback is not the database the hash describes, so it is never remembered
// and never skipped: every tick shells it again.
func TestControlReadyIdleGateNeverSkipsTheBdShellFallback(t *testing.T) {
	f := newIdleGateFixture(t)
	f.legErr = errors.New("ledger open failed")
	t.Setenv(citylayout.RealBdEnvVar, "")
	prevExe, prevRunner := controlReadyExecutable, controlReadyCommandRunner
	t.Cleanup(func() { controlReadyExecutable, controlReadyCommandRunner = prevExe, prevRunner })
	controlReadyExecutable = func() (string, error) { return "/opt/gascity/current/gc", nil }
	calls := 0
	controlReadyCommandRunner = func(string, []string, string, string, []string) (string, error) {
		calls++
		return "[]", nil
	}
	for i := 0; i < 3; i++ {
		if got := f.tick(); len(got) != 0 {
			t.Fatalf("tick %d: queue = %v, want empty", i, got)
		}
	}
	if calls != 3 {
		t.Fatalf("bd shell fallback ran %d times over 3 ticks, want 3", calls)
	}
}

// A work query that is not a control-ready query runs as a shell command on
// every tick; the gate neither answers nor remembers it.
func TestControlReadyIdleGatePassesNonControlQueriesToTheShell(t *testing.T) {
	f := newIdleGateFixture(t)
	calls := filepath.Join(t.TempDir(), "calls")
	query := "printf x >> " + calls + "; echo '[]'"
	for i := 0; i < 3; i++ {
		if _, err := nextWorkflowServeBeads(query, f.dir, nil); err != nil {
			t.Fatal(err)
		}
	}
	data, _ := os.ReadFile(calls)
	if len(data) != 3 {
		t.Fatalf("shell work query ran %d times over 3 ticks, want 3", len(data))
	}
	controlReadyIdleGate.mu.Lock()
	remembered := len(controlReadyIdleGate.byDir)
	controlReadyIdleGate.mu.Unlock()
	if remembered != 0 {
		t.Fatalf("gate remembered %d scopes for a shell query, want 0", remembered)
	}
}

// resetControlReadyVersionTargets isolates the target cache and its seams.
func resetControlReadyVersionTargets(t *testing.T) {
	t.Helper()
	prevResolve, prevCanonical, prevProjected, prevNow := controlReadyResolveVersionTargetFn, controlReadyCanonicalTargetFn, controlReadyProjectedScopeEnvFn, controlReadyNow
	reset := func() {
		controlReadyVersionTargets.mu.Lock()
		controlReadyVersionTargets.byDir = make(map[string]controlReadyVersionTargetEntry)
		controlReadyVersionTargets.failing = make(map[string]bool)
		controlReadyVersionTargets.mu.Unlock()
	}
	reset()
	t.Cleanup(func() {
		controlReadyResolveVersionTargetFn, controlReadyCanonicalTargetFn, controlReadyProjectedScopeEnvFn, controlReadyNow = prevResolve, prevCanonical, prevProjected, prevNow
		reset()
	})
}

// versionGuardScope is a city-scope directory whose canonical target and
// projected env are stubbed to a matching managed server, so only the guards
// under test can refuse it. canonicalCalls counts target resolutions.
func versionGuardScope(t *testing.T) (string, *int) {
	t.Helper()
	resetControlReadyVersionTargets(t)
	city := t.TempDir()
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname = \"guard\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BEADS", "")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	canonicalCalls := 0
	controlReadyCanonicalTargetFn = func(string, string) (contract.DoltConnectionTarget, bool, error) {
		canonicalCalls++
		return contract.DoltConnectionTarget{Port: "3307", Database: "hq"}, true, nil
	}
	controlReadyProjectedScopeEnvFn = func(string, string) (map[string]string, error) {
		return map[string]string{"GC_DOLT_PORT": "3307", "BEADS_DOLT_SERVER_PORT": "3307"}, nil
	}
	return city, &canonicalCalls
}

func TestControlReadyResolveVersionTargetAdmitsAManagedBdScope(t *testing.T) {
	city, _ := versionGuardScope(t)
	if _, ok := controlReadyResolveVersionTarget(city, city); !ok {
		t.Fatal("a managed bd scope whose projected env matches its target was refused")
	}
}

func TestControlReadyResolveVersionTargetRefusesNonBdProviders(t *testing.T) {
	for _, provider := range []string{"file", "exec:/opt/custom-beads.sh", "exec:/opt/gc-beads-bd.sh"} {
		t.Run(provider, func(t *testing.T) {
			city, canonicalCalls := versionGuardScope(t)
			t.Setenv("GC_BEADS", provider)
			if _, ok := controlReadyResolveVersionTarget(city, city); ok {
				t.Fatalf("provider %q was admitted", provider)
			}
			if *canonicalCalls != 0 {
				t.Fatalf("provider %q reached canonicalScopeDoltTarget %d times, want 0", provider, *canonicalCalls)
			}
		})
	}
}

func TestControlReadyResolveVersionTargetRefusesExternallyBoundScope(t *testing.T) {
	city, canonicalCalls := versionGuardScope(t)
	if err := os.MkdirAll(filepath.Join(city, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	binding := `{"backend":"dolt","storage_endpoint":"dolt://db.example:3306","storage_database":"hq"}`
	if err := os.WriteFile(scopeMetadataJSONPath(city), []byte(binding), 0o644); err != nil {
		t.Fatal(err)
	}
	if bound, err := scopeStoreIsExternallyBound(city, city); err != nil || !bound {
		t.Fatalf("fixture: scopeStoreIsExternallyBound = (%t, %v), want (true, nil)", bound, err)
	}
	if _, ok := controlReadyResolveVersionTarget(city, city); ok {
		t.Fatal("an externally bound scope was admitted")
	}
	if *canonicalCalls != 0 {
		t.Fatalf("externally bound scope reached canonicalScopeDoltTarget %d times, want 0", *canonicalCalls)
	}
}

// The endpoint check compares against the env the bd runner projects for the
// scope, not the dispatcher's ambient env.
func TestControlReadyResolveVersionTargetChecksTheProjectedEnv(t *testing.T) {
	city, _ := versionGuardScope(t)
	t.Setenv("GC_DOLT_PORT", "4400") // ambient: overridden by the projection
	if _, ok := controlReadyResolveVersionTarget(city, city); !ok {
		t.Fatal("a stale ambient port refused a scope whose projected env matches")
	}
	controlReadyProjectedScopeEnvFn = func(string, string) (map[string]string, error) {
		return map[string]string{"GC_DOLT_PORT": "4400", "BEADS_DOLT_SERVER_PORT": "4400"}, nil
	}
	if _, ok := controlReadyResolveVersionTarget(city, city); ok {
		t.Fatal("a scope whose projected env points bd elsewhere was admitted")
	}
	controlReadyProjectedScopeEnvFn = func(string, string) (map[string]string, error) {
		return nil, errors.New("projection failed")
	}
	if _, ok := controlReadyResolveVersionTarget(city, city); ok {
		t.Fatal("a scope whose projection failed was admitted")
	}
}

// Refused scopes are cached like admitted ones, so an unversionable scope
// does not re-read config every tick; both expire after the TTL.
func TestControlReadyVersionTargetCachesRefusalsUntilTTL(t *testing.T) {
	for _, admit := range []bool{false, true} {
		resetControlReadyVersionTargets(t)
		now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
		controlReadyNow = func() time.Time { return now }
		resolves := 0
		controlReadyResolveVersionTargetFn = func(string, string) (contract.DoltConnectionTarget, bool) {
			resolves++
			return contract.DoltConnectionTarget{Port: "3307", Database: "hq"}, admit
		}
		for i := 0; i < 5; i++ {
			if _, ok := controlReadyVersionTarget("/city", "/city"); ok != admit {
				t.Fatalf("admit=%t: ok = %t", admit, ok)
			}
			now = now.Add(workflowServeMaxIdleSleep)
		}
		if want := 1 + int(4*workflowServeMaxIdleSleep/controlReadyVersionTargetTTL); resolves != want {
			t.Fatalf("admit=%t: resolutions over 5 ticks = %d, want %d", admit, resolves, want)
		}
		now = now.Add(controlReadyVersionTargetTTL)
		controlReadyVersionTarget("/city", "/city")
		if resolves < 2 {
			t.Fatalf("admit=%t: target not re-resolved after the TTL", admit)
		}
	}
}

// A failing probe logs once on the transition and once on recovery, not on
// every tick, and drops the cached target each time.
func TestControlReadyProbeFailureLogsOnTransitionOnly(t *testing.T) {
	resetControlReadyVersionTargets(t)
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })
	resolves := 0
	controlReadyResolveVersionTargetFn = func(string, string) (contract.DoltConnectionTarget, bool) {
		resolves++
		return contract.DoltConnectionTarget{Port: "3307", Database: "hq"}, true
	}
	prevOpen := controlReadyOpenVersionDB
	t.Cleanup(func() { controlReadyOpenVersionDB = prevOpen })
	controlReadyOpenVersionDB = func(contract.DoltConnectionTarget) (*sql.DB, error) { return nil, errors.New("dial refused") }
	dir := t.TempDir()
	for i := 0; i < 4; i++ {
		if _, ok := controlReadyDoltStoreVersion(dir); ok {
			t.Fatal("a failed probe reported a version")
		}
	}
	if got := strings.Count(buf.String(), "dial refused"); got != 1 {
		t.Fatalf("probe-failure log lines over 4 failing ticks = %d, want 1:\n%s", got, buf.String())
	}
	if resolves != 4 {
		t.Fatalf("target resolutions = %d, want 4 (a failed probe drops the cached target)", resolves)
	}
	controlReadyNoteProbeResult(dir, nil)
	controlReadyNoteProbeResult(dir, nil)
	if got := strings.Count(buf.String(), "readable again"); got != 1 {
		t.Fatalf("recovery log lines = %d, want 1:\n%s", got, buf.String())
	}
}
