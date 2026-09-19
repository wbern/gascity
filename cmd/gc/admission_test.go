package main

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

func TestAdmissionRunner_Exit0IsAllow(t *testing.T) {
	ClearAdmissionCache()
	runnerCalled := 0
	mockRunner := func(_, _ string, _ time.Duration, _ map[string]string) (string, int, error) {
		runnerCalled++
		return "host load nominal 0.45", 0, nil
	}

	adm := config.AdmissionConfig{
		Check:   "check-load.sh",
		Timeout: "5s",
	}
	now := time.Now()
	verdict := evaluateAdmission(adm, "/city", nil, now, globalAdmissionCache, mockRunner)

	if runnerCalled != 1 {
		t.Errorf("runnerCalled = %d, want 1", runnerCalled)
	}
	if verdict.State != AdmissionAllow {
		t.Errorf("verdict.State = %v, want %v", verdict.State, AdmissionAllow)
	}
	if !verdict.Allowed {
		t.Errorf("verdict.Allowed = false, want true")
	}
	if verdict.Reason != "host load nominal 0.45" {
		t.Errorf("verdict.Reason = %q, want 'host load nominal 0.45'", verdict.Reason)
	}
	if verdict.ExitCode != 0 {
		t.Errorf("verdict.ExitCode = %d, want 0", verdict.ExitCode)
	}
}

func TestAdmissionRunner_Exit1IsDeny(t *testing.T) {
	ClearAdmissionCache()
	mockRunner := func(_, _ string, _ time.Duration, _ map[string]string) (string, int, error) {
		return "cpu pressure critical 98%", 1, errors.New("exit status 1")
	}

	adm := config.AdmissionConfig{
		Check:   "check-load.sh",
		Timeout: "5s",
	}
	now := time.Now()
	verdict := evaluateAdmission(adm, "/city", nil, now, globalAdmissionCache, mockRunner)

	if verdict.State != AdmissionDeny {
		t.Errorf("verdict.State = %v, want %v", verdict.State, AdmissionDeny)
	}
	if verdict.Allowed {
		t.Errorf("verdict.Allowed = true, want false")
	}
	if verdict.Reason != "cpu pressure critical 98%" {
		t.Errorf("verdict.Reason = %q, want 'cpu pressure critical 98%%'", verdict.Reason)
	}
	if verdict.ExitCode != 1 {
		t.Errorf("verdict.ExitCode = %d, want 1", verdict.ExitCode)
	}
}

func TestAdmissionRunner_OutputNeverParsedAsNumber(t *testing.T) {
	ClearAdmissionCache()
	// Even if stdout is a number, exit 1 is DENY (allowed=false), not 5 seats allowed.
	mockRunner := func(_, _ string, _ time.Duration, _ map[string]string) (string, int, error) {
		return "5\n", 1, errors.New("exit status 1")
	}

	adm := config.AdmissionConfig{
		Check:   "echo 5; exit 1",
		Timeout: "5s",
	}
	now := time.Now()
	verdict := evaluateAdmission(adm, "/city", nil, now, globalAdmissionCache, mockRunner)

	if verdict.Allowed {
		t.Errorf("verdict.Allowed = true, want false: output '5' with exit 1 must never be parsed as demand count")
	}
	if verdict.State != AdmissionDeny {
		t.Errorf("verdict.State = %v, want %v", verdict.State, AdmissionDeny)
	}
	if verdict.Reason != "5" {
		t.Errorf("verdict.Reason = %q, want '5'", verdict.Reason)
	}
}

func TestAdmissionRunner_Exit2IsErrorWithDefaultAllow(t *testing.T) {
	ClearAdmissionCache()
	mockRunner := func(_, _ string, _ time.Duration, _ map[string]string) (string, int, error) {
		return "command syntax error", 2, errors.New("exit status 2")
	}

	adm := config.AdmissionConfig{
		Check:   "broken-command",
		Timeout: "5s",
		OnError: "allow", // default
	}
	now := time.Now()
	verdict := evaluateAdmission(adm, "/city", nil, now, globalAdmissionCache, mockRunner)

	if verdict.State != AdmissionError {
		t.Errorf("verdict.State = %v, want %v", verdict.State, AdmissionError)
	}
	// Error is NOT deny: default on_error=allow prevents transient infra issues from zeroing demand.
	if !verdict.Allowed {
		t.Errorf("verdict.Allowed = false, want true when OnError=allow")
	}
	if verdict.Reason != "command syntax error" {
		t.Errorf("verdict.Reason = %q, want 'command syntax error'", verdict.Reason)
	}
	if verdict.ExitCode != 2 {
		t.Errorf("verdict.ExitCode = %d, want 2", verdict.ExitCode)
	}
}

func TestAdmissionRunner_Exit2IsErrorWithExplicitDeny(t *testing.T) {
	ClearAdmissionCache()
	mockRunner := func(_, _ string, _ time.Duration, _ map[string]string) (string, int, error) {
		return "infra timeout", -1, errors.New("context deadline exceeded")
	}

	adm := config.AdmissionConfig{
		Check:   "timeout-check",
		Timeout: "5s",
		OnError: "deny",
	}
	now := time.Now()
	verdict := evaluateAdmission(adm, "/city", nil, now, globalAdmissionCache, mockRunner)

	if verdict.State != AdmissionError {
		t.Errorf("verdict.State = %v, want %v", verdict.State, AdmissionError)
	}
	if verdict.Allowed {
		t.Errorf("verdict.Allowed = true, want false when OnError=deny")
	}
}

func TestAdmissionRunner_CacheOneSubprocessPerInterval(t *testing.T) {
	ClearAdmissionCache()
	var callCount int32
	mockRunner := func(_, _ string, _ time.Duration, _ map[string]string) (string, int, error) {
		atomic.AddInt32(&callCount, 1)
		return "ok", 0, nil
	}

	adm := config.AdmissionConfig{
		Check:    "shared-check.sh",
		Interval: "30s",
	}
	start := time.Now()

	// 5 queries at start time
	for i := 0; i < 5; i++ {
		v := evaluateAdmission(adm, "/city", nil, start, globalAdmissionCache, mockRunner)
		if i > 0 && !v.Cached {
			t.Errorf("iteration %d: expected cached=true", i)
		}
	}
	if count := atomic.LoadInt32(&callCount); count != 1 {
		t.Errorf("runner called %d times, want exactly 1 within interval", count)
	}

	// Query at start + 10s (still within 30s interval)
	v := evaluateAdmission(adm, "/city", nil, start.Add(10*time.Second), globalAdmissionCache, mockRunner)
	if !v.Cached {
		t.Errorf("expected cached=true at start + 10s")
	}
	if count := atomic.LoadInt32(&callCount); count != 1 {
		t.Errorf("runner called %d times, want 1", count)
	}

	// Query at start + 31s (expired TTL)
	v2 := evaluateAdmission(adm, "/city", nil, start.Add(31*time.Second), globalAdmissionCache, mockRunner)
	if v2.Cached {
		t.Errorf("expected cached=false at start + 31s (expired TTL)")
	}
	if count := atomic.LoadInt32(&callCount); count != 2 {
		t.Errorf("runner called %d times after expiration, want 2", count)
	}
}

func TestAdmissionRunner_DisabledWhenCheckEmptyOrOff(t *testing.T) {
	ClearAdmissionCache()
	runnerCalled := false
	mockRunner := func(_, _ string, _ time.Duration, _ map[string]string) (string, int, error) {
		runnerCalled = true
		return "bad", 1, errors.New("should not run")
	}

	v1 := evaluateAdmission(config.AdmissionConfig{Check: ""}, "/city", nil, time.Now(), globalAdmissionCache, mockRunner)
	if !v1.Allowed || v1.State != AdmissionAllow {
		t.Errorf("empty check: v1 = %+v, want allow", v1)
	}

	v2 := evaluateAdmission(config.AdmissionConfig{Check: "off"}, "/city", nil, time.Now(), globalAdmissionCache, mockRunner)
	if !v2.Allowed || v2.State != AdmissionAllow {
		t.Errorf("check=off: v2 = %+v, want allow", v2)
	}

	if runnerCalled {
		t.Errorf("mockRunner was called when admission was disabled")
	}
}

func TestResolveAdmissionVerdicts_CityGateSharedAcrossPools(t *testing.T) {
	ClearAdmissionCache()
	var callCount int32
	mockRunner := func(_, _ string, _ time.Duration, _ map[string]string) (string, int, error) {
		atomic.AddInt32(&callCount, 1)
		return "city pressure ok", 0, nil
	}

	cfg := &config.City{
		Admission: config.AdmissionConfig{
			Check:    "/shared/city-admission.sh",
			Interval: "30s",
		},
		Agents: []config.Agent{
			{Name: "worker1"},
			{Name: "worker2"},
			{Name: "worker3"},
		},
	}
	demandByTemplate := map[string]int{
		"worker1": 2,
		"worker2": 4,
		"worker3": 1,
	}

	verdicts := resolveAdmissionVerdicts(cfg, "/city", demandByTemplate, time.Now(), mockRunner, nil)
	if len(verdicts) != 3 {
		t.Fatalf("len(verdicts) = %d, want 3", len(verdicts))
	}
	for _, template := range []string{"worker1", "worker2", "worker3"} {
		v, ok := verdicts[template]
		if !ok {
			t.Errorf("missing verdict for %q", template)
		}
		if !v.Allowed || v.State != AdmissionAllow {
			t.Errorf("verdict for %q = %+v, want allow", template, v)
		}
	}
	// The city gate was evaluated once, and the other 2 pools hit the cache!
	if count := atomic.LoadInt32(&callCount); count != 1 {
		t.Errorf("city gate was executed %d times across 3 pools, want 1", count)
	}
}
