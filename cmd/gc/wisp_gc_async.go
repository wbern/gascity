package main

import (
	"context"
	"fmt"
	"log"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// wispGCResult is the outcome of one background wisp GC sweep.
type wispGCResult struct {
	purged   int
	err      error
	started  time.Time
	duration time.Duration
}

// wispGCSweepsByCity holds one entry per city path with a sweep in flight,
// across every CityRuntime in the process.
var wispGCSweepsByCity sync.Map

// wispGCContextRunner is implemented by wisp GC trackers whose sweep stops
// before its next arm once ctx is done.
type wispGCContextRunner interface {
	runGCContext(ctx context.Context, graphStore beads.GraphStore, mailStore beads.MailStore, now time.Time) (int, error)
}

// runWispGCWithContext runs one sweep, honoring ctx when the tracker supports
// it.
func runWispGCWithContext(ctx context.Context, wg wispGC, graphStore beads.GraphStore, mailStore beads.MailStore, now time.Time) (int, error) {
	if cr, ok := wg.(wispGCContextRunner); ok {
		return cr.runGCContext(ctx, graphStore, mailStore, now)
	}
	return wg.runGC(graphStore, mailStore, now)
}

// wispGCRunner runs wisp GC sweeps off the controller tick goroutine, at most
// one at a time. A sweep can take minutes on a large store; running it inline
// froze every order, patrol, and reconcile step for its duration. The tick
// launches a sweep and later harvests its result, so all logging and tracing
// stay on the tick goroutine. The zero value is ready to use.
type wispGCRunner struct {
	mu            sync.Mutex
	inFlight      bool
	stopping      bool
	abandoned     bool
	startedAt     time.Time
	lastStallWarn time.Time
	cancel        context.CancelFunc
	done          chan struct{}
	result        *wispGCResult
}

// start launches sweep in a background goroutine unless a sweep is already in
// flight or the runner is stopping. It reports whether a sweep was launched.
// A panicking sweep is recovered into the sweep's error, as safeTick recovered
// it when the sweep ran inline, so a store failure cannot crash the process.
func (r *wispGCRunner) start(now time.Time, sweep func(context.Context) (int, error)) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inFlight || r.stopping {
		return false
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	r.inFlight = true
	r.startedAt = now
	r.lastStallWarn = time.Time{}
	r.cancel = cancel
	r.done = done
	go func() {
		defer close(done)
		defer cancel()
		purged, err := runWispGCSweepRecovering(ctx, sweep)
		res := &wispGCResult{purged: purged, err: err, started: now, duration: time.Since(now)}
		r.mu.Lock()
		r.inFlight = false
		abandoned := r.abandoned
		if !abandoned {
			r.result = res
		}
		r.mu.Unlock()
		if abandoned {
			log.Printf("wisp gc: sweep abandoned at shutdown finished after %s (purged %d, err: %v); result not reported", res.duration.Round(time.Millisecond), res.purged, res.err)
		}
	}()
	return true
}

func runWispGCSweepRecovering(ctx context.Context, sweep func(context.Context) (int, error)) (purged int, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("sweep panicked: %v (type=%T)\n%s", p, p, debug.Stack())
		}
	}()
	return sweep(ctx)
}

// running reports whether a sweep is in flight.
func (r *wispGCRunner) running() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inFlight
}

// overdue reports how long the in-flight sweep has run once that exceeds
// limit, at most once per limit, so a hung sweep is visible instead of
// silently pausing GC.
func (r *wispGCRunner) overdue(now time.Time, limit time.Duration) (time.Duration, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.inFlight || limit <= 0 {
		return 0, false
	}
	age := now.Sub(r.startedAt)
	if age < limit {
		return 0, false
	}
	if !r.lastStallWarn.IsZero() && now.Sub(r.lastStallWarn) < limit {
		return 0, false
	}
	r.lastStallWarn = now
	return age, true
}

// harvest returns the result of the most recently finished sweep, at most once.
func (r *wispGCRunner) harvest() (wispGCResult, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.result == nil {
		return wispGCResult{}, false
	}
	res := *r.result
	r.result = nil
	return res, true
}

// stop refuses all future sweeps. An in-flight sweep keeps running.
func (r *wispGCRunner) stop() {
	r.mu.Lock()
	r.stopping = true
	r.mu.Unlock()
}

// abandon cancels the in-flight sweep, which then stops before its next arm,
// and drops its result. It reports how long the sweep had run and whether one
// was in flight.
func (r *wispGCRunner) abandon(now time.Time) (time.Duration, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.inFlight {
		return 0, false
	}
	r.abandoned = true
	r.cancel()
	return now.Sub(r.startedAt), true
}

// waitIdle blocks until no sweep is in flight or timeout elapses, reporting
// whether the runner is idle.
func (r *wispGCRunner) waitIdle(timeout time.Duration) bool {
	r.mu.Lock()
	inFlight, done := r.inFlight, r.done
	r.mu.Unlock()
	if !inFlight {
		return true
	}
	if timeout <= 0 {
		return false
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

// launchWispGC starts a background wisp GC sweep when one is due and none is
// in flight. The molecule/wisp/workflow purge arm routes through the typed
// graph-class store; the read-message retention arm through the typed
// messaging-class store. Both collapse to the city store today.
func (cr *CityRuntime) launchWispGC(now time.Time) {
	graphStore := cr.graphBeadStore()
	if cr.wg == nil || graphStore.Store == nil {
		return
	}
	if cr.wispSweeps.running() {
		cr.warnOverdueWispGC(now)
		return
	}
	if !cr.wg.shouldRun(now) {
		return
	}
	// A runtime replaced in-process (supervisor city restart) may leave its
	// sweep running past the bounded shutdown drain; never overlap it.
	cityKey := cr.cityPath
	if _, busy := wispGCSweepsByCity.LoadOrStore(cityKey, struct{}{}); busy {
		return
	}
	// Capture the tracker and stores on the tick goroutine: a config reload
	// may replace cr.wg while the sweep runs.
	wg, mailStore := cr.wg, cr.mailBeadStore()
	if !cr.wispSweeps.start(now, func(ctx context.Context) (int, error) {
		defer wispGCSweepsByCity.Delete(cityKey)
		return runWispGCWithContext(ctx, wg, graphStore, mailStore, now)
	}) {
		wispGCSweepsByCity.Delete(cityKey)
	}
}

// warnOverdueWispGC warns, rate-limited, when the in-flight sweep has run
// longer than twice the GC interval: new sweeps stay paused until it ends.
func (cr *CityRuntime) warnOverdueWispGC(now time.Time) {
	if cr.cfg == nil {
		return
	}
	age, ok := cr.wispSweeps.overdue(now, 2*cr.cfg.Daemon.WispGCIntervalDuration())
	if !ok {
		return
	}
	fmt.Fprintf(cr.stderr, "%s: wisp gc: sweep still running after %s; new sweeps are paused until it finishes\n", cr.logPrefix, age.Round(time.Second)) //nolint:errcheck // best-effort stderr
}

// reportWispGC logs and traces the outcome of a finished background sweep, if
// any. It must run on the tick goroutine (or after the tick loop has exited).
func (cr *CityRuntime) reportWispGC(trace *sessionReconcilerTraceCycle) {
	res, ok := cr.wispSweeps.harvest()
	if !ok {
		return
	}
	trace.RecordControllerOperation(TraceSiteControllerTickPhase, TraceReasonRetained, TraceOutcomeComplete, "wisp_gc", res.duration, map[string]any{
		"purged":     res.purged,
		"background": true,
		"started_at": res.started.UTC().Format(time.RFC3339),
	})
	if res.err != nil {
		for _, line := range strings.Split(res.err.Error(), "\n") {
			if line == "" {
				continue
			}
			fmt.Fprintf(cr.stderr, "%s: wisp gc: %s\n", cr.logPrefix, line) //nolint:errcheck // best-effort stderr
		}
	}
	fmt.Fprintf(cr.stdout, "%s: wisp gc: sweep finished in %s (purged %d)\n", cr.logPrefix, res.duration.Round(time.Millisecond), res.purged) //nolint:errcheck // best-effort stdout
	if res.purged > 0 {
		fmt.Fprintf(cr.stdout, "Bead GC: purged %d expired bead(s)\n", res.purged) //nolint:errcheck // best-effort stdout
	}
}

// drainWispGC stops launching sweeps and waits up to timeout for an in-flight
// sweep, reporting its outcome. A sweep still running at the deadline is
// abandoned: it stops before its next arm, so it does not keep issuing store
// calls while the bead store shuts down. It reports whether the runner
// drained.
func (cr *CityRuntime) drainWispGC(timeout time.Duration) bool {
	cr.wispSweeps.stop()
	drained := cr.wispSweeps.waitIdle(timeout)
	if !drained {
		if age, ok := cr.wispSweeps.abandon(time.Now()); ok {
			fmt.Fprintf(cr.stderr, "%s: wisp gc: abandoning sweep still running after %s drain (running for %s); it stops before its next arm and its result is not reported\n", cr.logPrefix, timeout, age.Round(time.Millisecond)) //nolint:errcheck // best-effort stderr
		}
	}
	cr.reportWispGC(nil)
	return drained
}

// drainWispGCForShutdown bounds the shutdown wait for an in-flight sweep the
// same way order dispatch is drained; a force stop does not wait at all.
func (cr *CityRuntime) drainWispGCForShutdown() {
	timeout := time.Duration(0)
	if cr.cfg != nil && !cr.forceStopRequested() {
		timeout = orderShutdownDrainTimeout(cr.cfg.Daemon.ShutdownTimeoutDuration())
	}
	cr.drainWispGC(timeout)
}

// wispGCArmTiming is how long one wisp GC arm took in a sweep.
type wispGCArmTiming struct {
	name string
	took time.Duration
}

// wispGCArmTimings records per-arm durations for one sweep so a slow sweep
// can be attributed to the arm that dominates it.
type wispGCArmTimings []wispGCArmTiming

func (t *wispGCArmTimings) since(name string, start time.Time) {
	*t = append(*t, wispGCArmTiming{name: name, took: time.Since(start)})
}

// String renders the timings as space-separated name=duration pairs.
func (t wispGCArmTimings) String() string {
	if len(t) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(t))
	for _, arm := range t {
		parts = append(parts, fmt.Sprintf("%s=%s", arm.name, arm.took.Round(time.Millisecond)))
	}
	return strings.Join(parts, " ")
}
