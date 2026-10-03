package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// blockingWispGC is a wispGC whose sweep blocks until release is closed, so a
// test can hold a sweep in flight across controller ticks.
type blockingWispGC struct {
	release chan struct{}
	started chan struct{}
	runs    atomic.Int32
	purged  int
	err     error
}

func newBlockingWispGC(purged int, err error) *blockingWispGC {
	return &blockingWispGC{
		release: make(chan struct{}),
		started: make(chan struct{}, 16),
		purged:  purged,
		err:     err,
	}
}

func (b *blockingWispGC) shouldRun(time.Time) bool { return true }

func (b *blockingWispGC) runGC(beads.GraphStore, beads.SessionStore, beads.MailStore, time.Time) (int, error) {
	b.runs.Add(1)
	b.started <- struct{}{}
	<-b.release
	return b.purged, b.err
}

func newWispGCTestRuntime(t *testing.T, wg wispGC, stdout, stderr *bytes.Buffer) *CityRuntime {
	t.Helper()
	return &CityRuntime{
		cityPath:            t.TempDir(),
		cityName:            "test-city",
		cfg:                 &config.City{},
		sp:                  runtime.NewFake(),
		standaloneCityStore: beads.NewMemStore(),
		wg:                  wg,
		rec:                 events.Discard,
		logPrefix:           "test-city",
		stdout:              stdout,
		stderr:              stderr,
		buildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
	}
}

func runTestTick(cr *CityRuntime) {
	var dirty atomic.Bool
	var lastProviderName string
	var prevPoolRunning map[string]bool
	cr.tick(context.Background(), &dirty, &lastProviderName, cr.cityPath, &prevPoolRunning, "test")
}

// waitForSweepStart fails the test unless the blocking sweep has begun.
func waitForSweepStart(t *testing.T, gc *blockingWispGC) {
	t.Helper()
	select {
	case <-gc.started:
	case <-time.After(5 * time.Second):
		t.Fatal("wisp gc sweep never started")
	}
}

// A slow wisp GC sweep must not hold the controller tick: the tick returns
// while the sweep is still running, later ticks never start a second
// concurrent sweep, and the finished sweep's outcome is reported by a later
// tick on the tick goroutine.
func TestCityRuntimeTick_SlowWispGCDoesNotBlockTick(t *testing.T) {
	gc := newBlockingWispGC(3, nil)
	var stdout, stderr bytes.Buffer
	cr := newWispGCTestRuntime(t, gc, &stdout, &stderr)
	t.Cleanup(func() {
		select {
		case <-gc.release:
		default:
			close(gc.release)
		}
		cr.drainWispGC(5 * time.Second)
	})

	tickDone := make(chan struct{})
	go func() {
		runTestTick(cr)
		close(tickDone)
	}()
	select {
	case <-tickDone:
	case <-time.After(5 * time.Second):
		t.Fatal("controller tick blocked on an in-flight wisp gc sweep")
	}
	waitForSweepStart(t, gc)

	// Further ticks while the sweep is in flight neither block nor start a
	// second sweep.
	for i := 0; i < 3; i++ {
		runTestTick(cr)
	}
	if got := gc.runs.Load(); got != 1 {
		t.Fatalf("wisp gc sweeps started while one was in flight = %d, want 1", got)
	}
	if strings.Contains(stdout.String(), "Bead GC: purged") {
		t.Fatalf("stdout = %q, reported a purge before the sweep finished", stdout.String())
	}

	close(gc.release)
	if !cr.wispSweeps.waitIdle(5 * time.Second) {
		t.Fatal("wisp gc sweep did not finish after release")
	}

	runTestTick(cr)
	if !strings.Contains(stdout.String(), "Bead GC: purged 3 expired bead(s)") {
		t.Fatalf("stdout = %q, want finished sweep reported on the next tick", stdout.String())
	}
	if !strings.Contains(stdout.String(), "test-city: wisp gc: sweep finished in ") {
		t.Fatalf("stdout = %q, want sweep duration line", stdout.String())
	}
	// The reporting tick is also free to start the next sweep.
	waitForSweepStart(t, gc)
	if got := gc.runs.Load(); got != 2 {
		t.Fatalf("wisp gc sweeps after the first finished = %d, want 2", got)
	}
}

// A sweep's outcome is reported exactly once, even across several ticks.
func TestCityRuntimeTick_ReportsWispGCResultOnce(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cr := newWispGCTestRuntime(t, onceWispGC{fixedWispGC: fixedWispGC{purged: 1}, ran: &atomic.Bool{}}, &stdout, &stderr)

	runTestTick(cr)
	if !cr.wispSweeps.waitIdle(5 * time.Second) {
		t.Fatal("wisp gc sweep did not finish")
	}
	runTestTick(cr)
	runTestTick(cr)
	if got := strings.Count(stdout.String(), "Bead GC: purged 1 expired bead(s)"); got != 1 {
		t.Fatalf("purge reported %d times, want 1; stdout=%q", got, stdout.String())
	}
}

// onceWispGC is due exactly once.
type onceWispGC struct {
	fixedWispGC
	ran *atomic.Bool
}

func (o onceWispGC) shouldRun(time.Time) bool { return !o.ran.Swap(true) }

// Shutdown reports a sweep that finishes within the bounded drain.
func TestCityRuntimeDrainWispGC_ReportsSweepFinishingWithinDrain(t *testing.T) {
	gc := newBlockingWispGC(0, fmt.Errorf("delete failed"))
	var stdout, stderr bytes.Buffer
	cr := newWispGCTestRuntime(t, gc, &stdout, &stderr)

	runTestTick(cr)
	waitForSweepStart(t, gc)
	close(gc.release)
	if !cr.drainWispGC(5 * time.Second) {
		t.Fatal("drainWispGC did not drain a sweep that finished")
	}
	if !strings.Contains(stderr.String(), "test-city: wisp gc: delete failed") {
		t.Fatalf("stderr = %q, want in-flight sweep error reported at drain", stderr.String())
	}
	if strings.Contains(stderr.String(), "abandoning sweep") {
		t.Fatalf("stderr = %q, a drained sweep must not be reported abandoned", stderr.String())
	}
}

// A sweep still running at the drain deadline is abandoned: its context is
// canceled so it stops issuing store calls, its late result is not reported,
// and no new sweeps start.
func TestCityRuntimeDrainWispGC_AbandonsSweepStillRunningAtDeadline(t *testing.T) {
	gc := newContextBlockingWispGC()
	var stdout, stderr bytes.Buffer
	cr := newWispGCTestRuntime(t, gc, &stdout, &stderr)

	runTestTick(cr)
	waitForSweepStart(t, &gc.blockingWispGC)

	if cr.drainWispGC(50 * time.Millisecond) {
		t.Fatal("drainWispGC reported drained while the sweep is still blocked")
	}
	if !strings.Contains(stderr.String(), "test-city: wisp gc: abandoning sweep still running after 50ms drain") {
		t.Fatalf("stderr = %q, want abandonment warning", stderr.String())
	}
	select {
	case <-gc.canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("abandoned sweep never observed its context cancellation")
	}
	if !cr.wispSweeps.waitIdle(5 * time.Second) {
		t.Fatal("abandoned sweep did not stop after cancellation")
	}
	if _, ok := cr.wispSweeps.harvest(); ok {
		t.Fatal("abandoned sweep's result was kept for reporting")
	}
	if _, busy := wispGCSweepsByCity.Load(cr.cityPath); busy {
		t.Fatal("abandoned sweep kept the per-city claim after stopping")
	}

	runTestTick(cr)
	if got := gc.runs.Load(); got != 1 {
		t.Fatalf("wisp gc sweeps after drain = %d, want 1 (no new sweeps once draining)", got)
	}
}

// contextBlockingWispGC blocks like blockingWispGC but returns as soon as its
// sweep context is canceled.
type contextBlockingWispGC struct {
	blockingWispGC
	canceled chan struct{}
}

func newContextBlockingWispGC() *contextBlockingWispGC {
	c := &contextBlockingWispGC{canceled: make(chan struct{})}
	c.release = make(chan struct{})
	c.started = make(chan struct{}, 16)
	return c
}

func (c *contextBlockingWispGC) runGCContext(ctx context.Context, _ beads.GraphStore, _ beads.SessionStore, _ beads.MailStore, _ time.Time) (int, error) {
	c.runs.Add(1)
	c.started <- struct{}{}
	select {
	case <-c.release:
		return c.purged, nil
	case <-ctx.Done():
		close(c.canceled)
		return c.purged, ctx.Err()
	}
}

// A panicking sweep must not crash the process (it ran under safeTick's
// recover when inline): the panic is reported as the sweep's error, the
// per-city claim and in-flight state are released, and the next interval
// sweeps again.
func TestCityRuntimeTick_PanickingWispGCSweepIsRecoveredAndReleased(t *testing.T) {
	gc := &panickingWispGC{}
	var stdout, stderr bytes.Buffer
	cr := newWispGCTestRuntime(t, gc, &stdout, &stderr)
	t.Cleanup(func() { cr.drainWispGC(5 * time.Second) })

	runTestTick(cr)
	if !cr.wispSweeps.waitIdle(5 * time.Second) {
		t.Fatal("panicking sweep left the runner in flight")
	}
	if _, busy := wispGCSweepsByCity.Load(cr.cityPath); busy {
		t.Fatal("panicking sweep kept the per-city claim")
	}

	runTestTick(cr)
	if !strings.Contains(stderr.String(), "test-city: wisp gc: sweep panicked: store exploded (type=string)") {
		t.Fatalf("stderr = %q, want recovered panic reported", stderr.String())
	}
	if !cr.wispSweeps.waitIdle(5 * time.Second) {
		t.Fatal("second sweep left the runner in flight")
	}
	if got := gc.runs.Load(); got != 2 {
		t.Fatalf("sweeps after a panic = %d, want 2 (next interval sweeps again)", got)
	}
}

type panickingWispGC struct{ runs atomic.Int32 }

func (p *panickingWispGC) shouldRun(time.Time) bool { return true }

func (p *panickingWispGC) runGC(beads.GraphStore, beads.SessionStore, beads.MailStore, time.Time) (int, error) {
	p.runs.Add(1)
	panic("store exploded")
}

// A hung sweep pauses GC for the city; the tick says so, at most once per
// limit, instead of skipping silently.
func TestCityRuntimeTick_WarnsWhenWispGCSweepIsOverdue(t *testing.T) {
	gc := newBlockingWispGC(0, nil)
	var stdout, stderr bytes.Buffer
	cr := newWispGCTestRuntime(t, gc, &stdout, &stderr)
	cr.cfg.Daemon.WispGCInterval = "1ms"
	t.Cleanup(func() {
		close(gc.release)
		cr.drainWispGC(5 * time.Second)
	})

	runTestTick(cr)
	waitForSweepStart(t, gc)
	time.Sleep(20 * time.Millisecond)
	runTestTick(cr)
	if !strings.Contains(stderr.String(), "test-city: wisp gc: sweep still running after ") ||
		!strings.Contains(stderr.String(), "new sweeps are paused until it finishes") {
		t.Fatalf("stderr = %q, want overdue sweep warning", stderr.String())
	}
}

func TestWispGCRunner_OverdueIsRateLimited(t *testing.T) {
	var r wispGCRunner
	release := make(chan struct{})
	t0 := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	r.start(t0, func(context.Context) (int, error) { <-release; return 0, nil })
	t.Cleanup(func() { close(release); r.waitIdle(5 * time.Second) })

	limit := 30 * time.Minute
	if _, ok := r.overdue(t0.Add(29*time.Minute), limit); ok {
		t.Fatal("overdue before the limit")
	}
	age, ok := r.overdue(t0.Add(31*time.Minute), limit)
	if !ok || age != 31*time.Minute {
		t.Fatalf("overdue at 31m = (%s, %v), want (31m, true)", age, ok)
	}
	if _, ok := r.overdue(t0.Add(40*time.Minute), limit); ok {
		t.Fatal("overdue warned again within one limit of the last warning")
	}
	if _, ok := r.overdue(t0.Add(62*time.Minute), limit); !ok {
		t.Fatal("overdue did not warn again once a full limit had passed")
	}
	if _, ok := r.overdue(t0.Add(200*time.Minute), 0); ok {
		t.Fatal("overdue with a zero limit warned")
	}
}

// The production tracker stops before its first arm when its context is
// already canceled.
func TestMemoryWispGCRunGCContextStopsWhenCancelled(t *testing.T) {
	store := beads.NewMemStore()
	wg := newWispGC(time.Minute, time.Hour, time.Hour).(*memoryWispGC)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	purged, err := wg.runGCContext(ctx, beads.GraphStore{Store: store}, beads.SessionStore{Store: store}, beads.MailStore{Store: store}, time.Now())
	if purged != 0 || err == nil || !strings.Contains(err.Error(), "sweep abandoned before spec_sidecars") {
		t.Fatalf("runGCContext(canceled) = (%d, %v), want (0, sweep abandoned before spec_sidecars)", purged, err)
	}
}

func TestWispGCRunner_SingleFlight(t *testing.T) {
	var r wispGCRunner
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	if !r.start(time.Now(), func(context.Context) (int, error) {
		defer wg.Done()
		<-release
		return 2, nil
	}) {
		t.Fatal("start on an idle runner = false, want true")
	}
	if !r.running() {
		t.Fatal("running() = false while a sweep is in flight")
	}
	if r.start(time.Now(), func(context.Context) (int, error) { t.Error("second concurrent sweep ran"); return 0, nil }) {
		t.Fatal("start while a sweep is in flight = true, want false")
	}
	if _, ok := r.harvest(); ok {
		t.Fatal("harvest returned a result while the sweep is in flight")
	}
	if r.waitIdle(20 * time.Millisecond) {
		t.Fatal("waitIdle = true while the sweep is blocked")
	}

	close(release)
	wg.Wait()
	if !r.waitIdle(5 * time.Second) {
		t.Fatal("waitIdle = false after the sweep finished")
	}
	res, ok := r.harvest()
	if !ok || res.purged != 2 || res.err != nil {
		t.Fatalf("harvest = (%+v, %v), want purged=2 err=nil", res, ok)
	}
	if res.duration <= 0 {
		t.Fatalf("harvest duration = %s, want > 0", res.duration)
	}
	if _, ok := r.harvest(); ok {
		t.Fatal("second harvest returned the same result again")
	}
	if r.running() {
		t.Fatal("running() = true after the sweep finished")
	}
}

func TestWispGCRunner_RefusesStartOnceStopping(t *testing.T) {
	var r wispGCRunner
	r.stop()
	if r.start(time.Now(), func(context.Context) (int, error) { t.Error("sweep ran after stop"); return 0, nil }) {
		t.Fatal("start after stop = true, want false")
	}
	if !r.waitIdle(0) {
		t.Fatal("waitIdle on an idle stopped runner = false, want true")
	}
}

// A replacement runtime for the same city must not start a sweep while the
// previous runtime's sweep is still running past its bounded shutdown drain.
func TestCityRuntimeTick_ReplacementRuntimeDoesNotOverlapSweepForSameCity(t *testing.T) {
	oldGC := newBlockingWispGC(0, nil)
	var stdout, stderr bytes.Buffer
	oldRuntime := newWispGCTestRuntime(t, oldGC, &stdout, &stderr)
	runTestTick(oldRuntime)
	waitForSweepStart(t, oldGC)
	if oldRuntime.drainWispGC(10 * time.Millisecond) {
		t.Fatal("old runtime drained while its sweep is blocked")
	}

	newGC := newBlockingWispGC(0, nil)
	newRuntime := newWispGCTestRuntime(t, newGC, &bytes.Buffer{}, &bytes.Buffer{})
	newRuntime.cityPath = oldRuntime.cityPath
	t.Cleanup(func() {
		select {
		case <-newGC.release:
		default:
			close(newGC.release)
		}
		newRuntime.drainWispGC(5 * time.Second)
	})

	runTestTick(newRuntime)
	if got := newGC.runs.Load(); got != 0 {
		t.Fatalf("replacement runtime started %d sweep(s) while the old sweep runs, want 0", got)
	}

	close(oldGC.release)
	if !oldRuntime.wispSweeps.waitIdle(5 * time.Second) {
		t.Fatal("old sweep did not finish after release")
	}
	runTestTick(newRuntime)
	waitForSweepStart(t, newGC)
}

// The production tracker and store run under the race detector: the sweep
// goroutine and later ticks share memoryWispGC and the store.
func TestCityRuntimeTick_BackgroundSweepWithProductionTrackerIsRaceFree(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cr := newWispGCTestRuntime(t, newWispGC(time.Nanosecond, time.Hour, time.Hour), &stdout, &stderr)
	t.Cleanup(func() { cr.drainWispGC(5 * time.Second) })
	for i := 0; i < 4; i++ {
		runTestTick(cr)
		if !cr.wispSweeps.waitIdle(5 * time.Second) {
			t.Fatal("wisp gc sweep did not finish")
		}
	}
	if got := strings.Count(stdout.String(), "test-city: wisp gc: sweep finished in "); got < 3 {
		t.Fatalf("finished sweeps reported = %d, want >= 3; stdout=%q stderr=%q", got, stdout.String(), stderr.String())
	}
}

func TestWispGCArmTimingsString(t *testing.T) {
	var arms wispGCArmTimings
	if got := arms.String(); got != "none" {
		t.Fatalf("empty timings = %q, want none", got)
	}
	arms = wispGCArmTimings{{name: "generated_members", took: 1500 * time.Millisecond}, {name: "orphan_reap", took: 2 * time.Millisecond}}
	if got, want := arms.String(), "generated_members=1.5s orphan_reap=2ms"; got != want {
		t.Fatalf("timings = %q, want %q", got, want)
	}
}

// Every sweep logs one per-arm timing line, so devops can attribute a slow
// sweep to the arm that dominates it.
func TestMemoryWispGCRunGCLogsArmTimings(t *testing.T) {
	var logBuf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&logBuf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })

	store := beads.NewMemStore()
	wg := newWispGC(time.Minute, time.Hour, time.Hour)
	if _, err := wg.runGC(beads.GraphStore{Store: store}, beads.SessionStore{Store: store}, beads.MailStore{Store: store}, time.Now()); err != nil {
		t.Fatalf("runGC: %v", err)
	}
	line := ""
	for _, l := range strings.Split(logBuf.String(), "\n") {
		if strings.HasPrefix(l, "wisp gc: sweep arm timings: ") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("log = %q, want a sweep arm timings line", logBuf.String())
	}
	for _, arm := range []string{"spec_sidecars=", "generated_members=", "abandoned_roots=", "closed_roots_list=", "closure_purge=", "orphan_reap=", "mail_retention="} {
		if !strings.Contains(line, arm) {
			t.Fatalf("timings line %q missing %s", line, arm)
		}
	}
}
