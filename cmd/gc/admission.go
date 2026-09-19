package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

// AdmissionVerdictState represents the categorical state of an admission check verdict.
type AdmissionVerdictState string

const (
	// AdmissionAllow indicates the admission check exited 0 (allow new demand).
	AdmissionAllow AdmissionVerdictState = "allow"
	// AdmissionDeny indicates the admission check exited 1 (deny new demand).
	AdmissionDeny AdmissionVerdictState = "deny"
	// AdmissionError indicates the admission check failed, timed out, or exited > 1.
	AdmissionError AdmissionVerdictState = "error"
)

// AdmissionVerdict is the result of evaluating a boolean admission gate.
type AdmissionVerdict struct {
	State       AdmissionVerdictState `json:"state"`
	Allowed     bool                  `json:"allowed"`
	Reason      string                `json:"reason,omitempty"`
	Duration    time.Duration         `json:"duration"`
	EvaluatedAt time.Time             `json:"evaluated_at"`
	Cached      bool                  `json:"cached"`
	ExitCode    int                   `json:"exit_code"`
}

type admissionCacheEntry struct {
	verdict   AdmissionVerdict
	expiresAt time.Time
}

type admissionCache struct {
	mu      sync.Mutex
	entries map[string]admissionCacheEntry
}

var globalAdmissionCache = &admissionCache{
	entries: make(map[string]admissionCacheEntry),
}

// ClearAdmissionCache clears the admission cache (primarily for tests).
func ClearAdmissionCache() {
	globalAdmissionCache.mu.Lock()
	defer globalAdmissionCache.mu.Unlock()
	globalAdmissionCache.entries = make(map[string]admissionCacheEntry)
}

// admissionRunnerFn is the signature for executing an admission check subprocess.
type admissionRunnerFn func(command, dir string, timeout time.Duration, env map[string]string) (string, int, error)

// execAdmissionCommand executes an admission command via sh -c with timeout and merged environment.
// It returns combined stdout/stderr, the process exit code, and any execution error.
func execAdmissionCommand(command, dir string, timeout time.Duration, env map[string]string) (string, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.WaitDelay = 2 * time.Second
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = mergeRuntimeEnv(os.Environ(), env)

	var combined bytes.Buffer
	cmd.Stdout = &combined
	cmd.Stderr = &combined

	err := cmd.Run()
	output := strings.TrimSpace(combined.String())
	if err == nil {
		return output, 0, nil
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return output, exitErr.ExitCode(), err
	}
	return output, -1, err
}

// shellAdmissionCheck is the default admission runner.
func shellAdmissionCheck(command, dir string, timeout time.Duration, env map[string]string) (string, int, error) {
	return execAdmissionCommand(command, dir, timeout, env)
}

// evaluateAdmission evaluates admission settings using the provided runner and cache.
func evaluateAdmission(
	adm config.AdmissionConfig,
	dir string,
	env map[string]string,
	now time.Time,
	cache *admissionCache,
	runner admissionRunnerFn,
) AdmissionVerdict {
	check := strings.TrimSpace(adm.Check)
	if check == "" || strings.EqualFold(check, "off") {
		return AdmissionVerdict{
			State:       AdmissionAllow,
			Allowed:     true,
			EvaluatedAt: now,
		}
	}

	timeout := adm.TimeoutDuration()
	interval := adm.IntervalDuration()
	onError := adm.OnErrorMode()

	cacheKey := dir + "\x00" + check
	if cache != nil {
		cache.mu.Lock()
		if entry, ok := cache.entries[cacheKey]; ok {
			if now.Before(entry.expiresAt) {
				cached := entry.verdict
				cached.Cached = true
				cache.mu.Unlock()
				return cached
			}
		}
		cache.mu.Unlock()
	}

	if runner == nil {
		runner = shellAdmissionCheck
	}

	start := time.Now()
	out, exitCode, err := runner(check, dir, timeout, env)
	duration := time.Since(start)

	var verdict AdmissionVerdict
	verdict.Duration = duration
	verdict.EvaluatedAt = now
	verdict.ExitCode = exitCode
	verdict.Reason = strings.TrimSpace(out)

	switch {
	case err == nil && exitCode == 0:
		verdict.State = AdmissionAllow
		verdict.Allowed = true
	case exitCode == 1:
		verdict.State = AdmissionDeny
		verdict.Allowed = false
		if verdict.Reason == "" {
			verdict.Reason = "admission gate denied (exit code 1)"
		}
	default:
		verdict.State = AdmissionError
		if onError == config.AdmissionOnErrorDeny {
			verdict.Allowed = false
		} else {
			// default: allow
			verdict.Allowed = true
		}
		if verdict.Reason == "" && err != nil {
			verdict.Reason = err.Error()
		}
	}

	if cache != nil {
		cache.mu.Lock()
		cache.entries[cacheKey] = admissionCacheEntry{
			verdict:   verdict,
			expiresAt: now.Add(interval),
		}
		cache.mu.Unlock()
	}

	return verdict
}

// resolveAdmissionVerdicts resolves admission verdicts for all templates in cfg.
// It uses globalAdmissionCache and evaluates checks only for templates that have demand.
// A city gate invocation is shared across all pools using that city check.
func resolveAdmissionVerdicts(
	cfg *config.City,
	cityPath string,
	demandByTemplate map[string]int,
	now time.Time,
	runner admissionRunnerFn,
	trace *sessionReconcilerTraceCycle,
) map[string]AdmissionVerdict {
	if cfg == nil {
		return nil
	}
	verdicts := make(map[string]AdmissionVerdict)
	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		if agent.Suspended {
			continue
		}
		template := agent.QualifiedName()
		demand := demandByTemplate[template]
		if demand <= 0 {
			// No demand: gate evaluation not needed for this tick.
			continue
		}

		adm, enabled := agent.EffectiveAdmission(cfg.Admission)
		if !enabled {
			continue
		}

		dir := cityPath
		if agent.Dir != "" && agent.WorkDir == "" {
			dir = agent.Dir
		} else if agent.WorkDir != "" {
			dir = agent.WorkDir
		}

		verdict := evaluateAdmission(adm, dir, agent.Env, now, globalAdmissionCache, runner)
		verdicts[template] = verdict

		if trace != nil {
			var outcome TraceOutcomeCode
			switch verdict.State {
			case AdmissionAllow:
				outcome = TraceOutcomeAllow
			case AdmissionDeny:
				outcome = TraceOutcomeDeny
			case AdmissionError:
				outcome = TraceOutcomeError
			default:
				outcome = TraceOutcomeUnknown
			}
			trace.RecordOperation(TraceSiteAdmissionCheckExec, TraceReasonAdmissionGate, outcome, "", template, "", verdict.Duration, traceRecordPayload{
				"command":        adm.Check,
				"verdict":        string(verdict.State),
				"allowed":        verdict.Allowed,
				"reason":         verdict.Reason,
				"exit_code":      verdict.ExitCode,
				"duration_ms":    verdict.Duration.Milliseconds(),
				"cached":         verdict.Cached,
				"agent_template": template,
				"on_error":       adm.OnErrorMode(),
			})
		}
	}
	return verdicts
}
