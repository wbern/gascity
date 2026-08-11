package main

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

type statusProbeProvider struct {
	runtime.Provider
	delay       atomic.Int64
	running     atomic.Bool
	liveness    atomic.Value
	observeCall atomic.Int32
}

func newStatusProbeProvider() *statusProbeProvider {
	p := &statusProbeProvider{Provider: runtime.NewFake()}
	p.liveness.Store(runtime.Liveness{})
	return p
}

func (p *statusProbeProvider) IsRunning(string) bool {
	time.Sleep(time.Duration(p.delay.Load()))
	return p.running.Load()
}

func (p *statusProbeProvider) ObserveLiveness(string, []string) runtime.Liveness {
	p.observeCall.Add(1)
	return p.liveness.Load().(runtime.Liveness)
}

func TestStatusProviderTimeoutDoesNotStickAcrossCalls(t *testing.T) {
	origTimeout := statusProviderCallTimeout
	origWarn := statusProviderTimeoutWarning
	t.Cleanup(func() {
		statusProviderCallTimeout = origTimeout
		statusProviderTimeoutWarning = origWarn
	})
	statusProviderCallTimeout = 10 * time.Millisecond
	var warnings atomic.Int32
	statusProviderTimeoutWarning = func() {
		warnings.Add(1)
	}

	base := newStatusProbeProvider()
	base.running.Store(true)
	base.delay.Store(int64(100 * time.Millisecond))
	wrapped := newBoundedStatusProvider(base)

	if wrapped.IsRunning("worker") {
		t.Fatal("first IsRunning returned true, want timeout fallback false")
	}
	base.delay.Store(0)
	if !wrapped.IsRunning("worker") {
		t.Fatal("second IsRunning returned false, want fresh provider result after timeout")
	}
	if got := warnings.Load(); got != 1 {
		t.Fatalf("timeout warnings = %d, want 1", got)
	}
}

func TestStatusProviderPreservesNativeLivenessObservation(t *testing.T) {
	base := newStatusProbeProvider()
	base.liveness.Store(runtime.Liveness{Running: true, Alive: true})
	wrapped := newBoundedStatusProvider(base)

	got := runtime.ObserveLiveness(wrapped, "worker", []string{"agent"})
	if !got.Running || !got.Alive {
		t.Fatalf("ObserveLiveness = %#v, want running+alive from native observer", got)
	}
	if calls := base.observeCall.Load(); calls != 1 {
		t.Fatalf("ObserveLiveness calls = %d, want 1", calls)
	}
}

func TestStatusProviderTimeoutMarksPartial(t *testing.T) {
	origTimeout := statusProviderCallTimeout
	origWarn := statusProviderTimeoutWarning
	t.Cleanup(func() {
		statusProviderCallTimeout = origTimeout
		statusProviderTimeoutWarning = origWarn
	})
	statusProviderCallTimeout = 10 * time.Millisecond
	statusProviderTimeoutWarning = func() {}

	base := newStatusProbeProvider()
	base.running.Store(true)
	base.delay.Store(int64(100 * time.Millisecond))
	wrapped := newBoundedStatusProvider(base)

	if wrapped.IsRunning("worker") {
		t.Fatal("IsRunning returned true, want timeout fallback false")
	}
	if !statusProviderPartial(wrapped) {
		t.Fatal("statusProviderPartial = false, want true after runtime probe timeout")
	}
}

func TestStatusProviderInstanceTimeoutCanExceedStatusBudget(t *testing.T) {
	origTimeout, origWarn := statusProviderCallTimeout, statusProviderTimeoutWarning
	defer func() { statusProviderCallTimeout, statusProviderTimeoutWarning = origTimeout, origWarn }()
	statusProviderCallTimeout = 10 * time.Millisecond
	timedOut := make(chan struct{}, 2)
	statusProviderTimeoutWarning = func() {
		select {
		case timedOut <- struct{}{}:
		default:
		}
	}
	ordinaryGate := newLivenessGateProvider()
	t.Cleanup(ordinaryGate.Release)
	ordinary := newBoundedStatusProvider(ordinaryGate)
	ordinaryResult := make(chan runtime.Liveness, 1)
	go func() {
		ordinaryResult <- ordinary.(interface {
			ObserveLiveness(string, []string) runtime.Liveness
		}).ObserveLiveness("worker", nil)
	}()
	doctorGate := newLivenessGateProvider()
	t.Cleanup(doctorGate.Release)
	doctor := newBoundedStatusProviderWithTimeout(doctorGate, time.Second)
	doctorResult := make(chan runtime.Liveness, 1)
	go func() {
		doctorResult <- doctor.(interface {
			ObserveLiveness(string, []string) runtime.Liveness
		}).ObserveLiveness("worker", nil)
	}()
	mustReceive(t, ordinaryGate.started, "ordinary probe start")
	mustReceive(t, doctorGate.started, "Doctor probe start")
	mustReceive(t, timedOut, "ordinary probe timeout")
	if got := mustReceive(t, ordinaryResult, "ordinary timeout result"); got.Running || !statusProviderPartial(ordinary) {
		t.Fatalf("ordinary result = %#v partial=%v, want timeout", got, statusProviderPartial(ordinary))
	}
	ordinaryGate.Release()
	statusProviderTimeoutWarning = func() {}
	doctorGate.Release()
	if got := mustReceive(t, doctorResult, "Doctor complete result"); !got.Running || statusProviderPartial(doctor) {
		t.Fatalf("Doctor result = %#v partial=%v, want complete", got, statusProviderPartial(doctor))
	}
}

func TestStatusProviderExplicitBudgetStillFailsClosed(t *testing.T) {
	gate := newLivenessGateProvider()
	t.Cleanup(gate.Release)
	wrapped := newBoundedStatusProviderWithTimeout(gate, 10*time.Millisecond)
	result := make(chan runtime.Liveness, 1)
	go func() {
		result <- wrapped.(interface {
			ObserveLiveness(string, []string) runtime.Liveness
		}).ObserveLiveness("worker", nil)
	}()
	mustReceive(t, gate.started, "explicit probe start")
	if got := mustReceive(t, result, "explicit timeout result"); got.Running || !statusProviderPartial(wrapped) {
		t.Fatalf("explicit-budget result = %#v partial=%v, want fail-closed timeout", got, statusProviderPartial(wrapped))
	}
	gate.Release()
}

type livenessGateProvider struct {
	runtime.Provider
	started     chan struct{}
	release     chan struct{}
	releaseOnce sync.Once
	value       runtime.Liveness
}

func (p *livenessGateProvider) Release() { p.releaseOnce.Do(func() { close(p.release) }) }

func (p *livenessGateProvider) ObserveLiveness(string, []string) runtime.Liveness {
	close(p.started)
	<-p.release
	return p.value
}

func newLivenessGateProvider() *livenessGateProvider {
	return &livenessGateProvider{Provider: runtime.NewFake(), started: make(chan struct{}), release: make(chan struct{}), value: runtime.Liveness{Running: true, Alive: true}}
}

func mustReceive[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
	var zero T
	return zero
}
