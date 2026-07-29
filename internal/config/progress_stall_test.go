package config

import (
	"testing"
	"time"
)

func TestProgressStallTimeoutDuration(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{"unset disables (zero)", "", 0},
		{"valid duration", "30m", 30 * time.Minute},
		{"too small clamps to safety floor", "30s", ProgressStallTimeoutMinimum},
		{"unparseable disables (zero)", "not-a-duration", 0},
		{"negative disables (zero)", "-5m", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &SessionConfig{ProgressStallTimeout: tc.value}
			if got := s.ProgressStallTimeoutDuration(); got != tc.want {
				t.Errorf("ProgressStallTimeoutDuration(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

func TestClaimHolderStallTimeoutDuration(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{"unset disables (zero)", "", 0},
		{"valid duration", "20m", 20 * time.Minute},
		{"too small clamps to safety floor", "1m", ProgressStallTimeoutMinimum},
		{"unparseable disables (zero)", "not-a-duration", 0},
		{"negative disables (zero)", "-5m", 0},
		// A padded value does not parse, so it disables — the same direction
		// the sibling progress_stall_timeout takes, and the doctor's
		// ValidateDurations check reports it rather than leaving it silent.
		{"padded value disables (zero)", "  20m  ", 0},
		{"whitespace-only disables (zero)", "   ", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &SessionConfig{ClaimHolderStallTimeout: tc.value}
			if got := s.ClaimHolderStallTimeoutDuration(); got != tc.want {
				t.Errorf("ClaimHolderStallTimeoutDuration(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

// TestAgentEffectiveClaimHolderStallTimeout covers the per-agent override of the
// city-wide claim-holder stall timeout. One city-wide threshold cannot serve both
// fungible pool workers (whose legitimate quiet period is minutes) and long-lived
// human-driven overseer sessions (whose quiet period between human turns is
// unbounded by design): a threshold tuned for the former recycles the latter every
// cycle, and because the restart cannot supply the missing human input the
// condition reproduces immediately and re-fires forever. The agent-level override
// is how a city expresses that difference.
//
// An unset override inherits the city value, so existing cities are unaffected.
func TestAgentEffectiveClaimHolderStallTimeout(t *testing.T) {
	tests := []struct {
		name        string
		agentValue  string
		cityDefault time.Duration
		want        time.Duration
	}{
		{"unset inherits city default", "", 30 * time.Minute, 30 * time.Minute},
		{"unset inherits disabled city default", "", 0, 0},
		{"whitespace-only inherits city default", "   ", 30 * time.Minute, 30 * time.Minute},
		{"agent value overrides city default", "8h", 30 * time.Minute, 8 * time.Hour},
		{"agent zero disables for this agent", "0", 30 * time.Minute, 0},
		{"agent negative disables for this agent", "-5m", 30 * time.Minute, 0},
		{"agent unparseable disables for this agent", "not-a-duration", 30 * time.Minute, 0},
		{"agent value clamps to safety floor", "1m", 30 * time.Minute, ProgressStallTimeoutMinimum},
		{"agent enables where city disabled", "45m", 0, 45 * time.Minute},
		// Whitespace-only is "unset" and inherits, but a padded value is a set
		// override that fails to parse, so it disables for this agent — the
		// same semantics the city knob applies.
		{"agent padded value disables for this agent", "  8h  ", 30 * time.Minute, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := &Agent{Name: "worker", ClaimHolderStallTimeout: tc.agentValue}
			if got := a.EffectiveClaimHolderStallTimeout(tc.cityDefault); got != tc.want {
				t.Errorf("EffectiveClaimHolderStallTimeout(%q, %v) = %v, want %v", tc.agentValue, tc.cityDefault, got, tc.want)
			}
		})
	}
}

// TestAgentEffectiveClaimHolderStallTimeoutNilAgent proves the resolver is safe
// for a session whose template resolves to no configured agent: it must fall back
// to the city default rather than panic or silently disable the recycler.
func TestAgentEffectiveClaimHolderStallTimeoutNilAgent(t *testing.T) {
	var a *Agent
	if got := a.EffectiveClaimHolderStallTimeout(30 * time.Minute); got != 30*time.Minute {
		t.Errorf("EffectiveClaimHolderStallTimeout on nil agent = %v, want 30m", got)
	}
}
