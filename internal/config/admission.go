package config

import (
	"fmt"
	"strings"
	"time"
)

const (
	// DefaultAdmissionTimeout is the default timeout for admission check execution.
	DefaultAdmissionTimeout = 10 * time.Second
	// DefaultAdmissionInterval is the default TTL / caching duration for an admission verdict.
	DefaultAdmissionInterval = 30 * time.Second
	// AdmissionOnErrorAllow indicates that an execution error or timeout falls back to allow.
	AdmissionOnErrorAllow = "allow"
	// AdmissionOnErrorDeny indicates that an execution error or timeout falls back to deny.
	AdmissionOnErrorDeny = "deny"
)

// AdmissionConfig defines boolean admission gate settings for host pressure control.
type AdmissionConfig struct {
	// Check is the shell command run to evaluate admission.
	// Exit 0 = allow, Exit 1 = deny, other/timeout = error.
	// When empty or "off", the admission gate is disabled (always allow).
	Check string `toml:"check,omitempty" json:"check,omitempty"`
	// Timeout bounds how long the admission check may run.
	// Duration string (e.g. "10s"). Default is 10s.
	Timeout string `toml:"timeout,omitempty" json:"timeout,omitempty"`
	// Interval defines the TTL / cache duration for a verdict before re-running.
	// Duration string (e.g. "30s"). Default is 30s.
	Interval string `toml:"interval,omitempty" json:"interval,omitempty"`
	// OnError determines the verdict when the check returns an error, times out,
	// or exits with code > 1. "allow" (default) or "deny".
	OnError string `toml:"on_error,omitempty" json:"on_error,omitempty" jsonschema:"enum=allow,enum=deny"`
}

func cloneAdmissionConfig(c *AdmissionConfig) *AdmissionConfig {
	if c == nil {
		return nil
	}
	cp := *c
	return &cp
}

// TimeoutDuration returns the parsed Timeout duration, or DefaultAdmissionTimeout (10s) if empty or unparseable.
func (c *AdmissionConfig) TimeoutDuration() time.Duration {
	if c == nil || strings.TrimSpace(c.Timeout) == "" {
		return DefaultAdmissionTimeout
	}
	d, err := time.ParseDuration(c.Timeout)
	if err != nil || d <= 0 {
		return DefaultAdmissionTimeout
	}
	return d
}

// IntervalDuration returns the parsed Interval duration, or DefaultAdmissionInterval (30s) if empty or unparseable.
func (c *AdmissionConfig) IntervalDuration() time.Duration {
	if c == nil || strings.TrimSpace(c.Interval) == "" {
		return DefaultAdmissionInterval
	}
	d, err := time.ParseDuration(c.Interval)
	if err != nil || d <= 0 {
		return DefaultAdmissionInterval
	}
	return d
}

// OnErrorMode returns the error fallback policy: "allow" (default) or "deny".
func (c *AdmissionConfig) OnErrorMode() string {
	if c == nil {
		return AdmissionOnErrorAllow
	}
	mode := strings.ToLower(strings.TrimSpace(c.OnError))
	if mode == AdmissionOnErrorDeny {
		return AdmissionOnErrorDeny
	}
	return AdmissionOnErrorAllow
}

// Validate checks the admission configuration fields for syntactic correctness.
func (c *AdmissionConfig) Validate(scope string) error {
	if c == nil {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(c.OnError)) {
	case "", AdmissionOnErrorAllow, AdmissionOnErrorDeny:
		// valid
	default:
		return fmt.Errorf("%s: admission.on_error must be %q, %q, or empty, got %q", scope, AdmissionOnErrorAllow, AdmissionOnErrorDeny, c.OnError)
	}
	return nil
}

// EffectiveAdmission returns the effective admission configuration for an agent,
// resolving field-wise agent-level overrides over the city-level admission settings.
// Returns (effectiveConfig, enabled).
// If the effective check is empty or "off", enabled is false.
func (a *Agent) EffectiveAdmission(cityAdmission AdmissionConfig) (AdmissionConfig, bool) {
	eff := cityAdmission
	if a != nil && a.Admission != nil {
		if a.Admission.Check != "" {
			eff.Check = a.Admission.Check
		}
		if a.Admission.Timeout != "" {
			eff.Timeout = a.Admission.Timeout
		}
		if a.Admission.Interval != "" {
			eff.Interval = a.Admission.Interval
		}
		if a.Admission.OnError != "" {
			eff.OnError = a.Admission.OnError
		}
	}
	check := strings.TrimSpace(eff.Check)
	if check == "" || strings.EqualFold(check, "off") {
		return AdmissionConfig{}, false
	}
	return eff, true
}
