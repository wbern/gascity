package config

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// OutputFirewallConfig is the operator-owned policy applied to managed read output.
type OutputFirewallConfig struct {
	// Enabled controls whether managed known-read output is bounded; default true.
	Enabled *bool `toml:"enabled,omitempty"`
	// ByteBudget is the maximum serialized stdout bytes for a managed read; default 32768.
	ByteBudget *int `toml:"byte_budget,omitempty"`
	// MaxByteBudget is the city-owned ceiling for per-agent output_firewall_byte_budget values.
	// Operators should normally keep a single command below roughly 10% of that
	// agent's context window: dense JSON is roughly 3.5–4 bytes per token, so
	// 32 KiB is about 8–9K tokens, 64 KiB about 16–19K, and 512 KiB about
	// 130–150K tokens. When unset, the ceiling is ByteBudget (or 32768).
	MaxByteBudget *int `toml:"max_byte_budget,omitempty"`
	// ReadVerbs is the closed allowlist of managed read routes to protect; default show, ready, list, query, mol, hook.
	ReadVerbs []string `toml:"read_verbs,omitempty"`
	// SpillMode selects secure, disabled, or required evidence retention; default secure.
	SpillMode string `toml:"spill_mode,omitempty"`
	// SpillPath is the city-relative directory for protected evidence artifacts; default .gc/evidence/output.
	SpillPath string `toml:"spill_path,omitempty"`
	// RetentionTTL is how long protected evidence artifacts are retained; default 24h.
	RetentionTTL string `toml:"retention_ttl,omitempty"`
}

// EffectiveByteBudget returns the managed-output budget.
func (c OutputFirewallConfig) EffectiveByteBudget() int {
	if c.ByteBudget == nil {
		return 32 << 10
	}
	return *c.ByteBudget
}

// EffectiveAgentByteBudget returns the controller-resolved budget for an agent.
// An explicit agent budget takes precedence over the city default and is capped
// by the city-owned maximum when one is configured.
func (c OutputFirewallConfig) EffectiveAgentByteBudget(agentBudget *int) int {
	budget := c.EffectiveByteBudget()
	if agentBudget != nil {
		budget = *agentBudget
	}
	maxBudget := c.EffectiveByteBudget()
	if c.MaxByteBudget != nil {
		maxBudget = *c.MaxByteBudget
	}
	if budget > maxBudget {
		return maxBudget
	}
	return budget
}

// EnabledForManagedSessions reports whether managed known reads are protected.
func (c OutputFirewallConfig) EnabledForManagedSessions() bool {
	return c.Enabled == nil || *c.Enabled
}

// EffectiveSpillMode returns the configured spill mode with its safe default.
func (c OutputFirewallConfig) EffectiveSpillMode() string {
	if c.SpillMode == "" {
		return "secure"
	}
	return c.SpillMode
}

// EffectiveReadVerbs returns the closed managed-read allowlist.
func (c OutputFirewallConfig) EffectiveReadVerbs() []string {
	if len(c.ReadVerbs) == 0 {
		return []string{"show", "ready", "list", "query", "mol", "hook"}
	}
	return append([]string(nil), c.ReadVerbs...)
}

// EffectiveSpillPath returns the city-relative artifact directory.
func (c OutputFirewallConfig) EffectiveSpillPath() string {
	if c.SpillPath == "" {
		return ".gc/evidence/output"
	}
	return c.SpillPath
}

// EffectiveRetentionTTL returns the artifact retention duration.
func (c OutputFirewallConfig) EffectiveRetentionTTL() time.Duration {
	if c.RetentionTTL == "" {
		return 24 * time.Hour
	}
	d, _ := time.ParseDuration(c.RetentionTTL)
	return d
}

// ValidateOutputFirewall rejects unsafe or semantically invalid city policy.
func ValidateOutputFirewall(cfg *City) error {
	if cfg == nil {
		return nil
	}
	c := cfg.OutputFirewall
	if c.ByteBudget != nil && *c.ByteBudget < 0 {
		return fmt.Errorf("output_firewall.byte_budget must be positive")
	}
	if c.ByteBudget != nil && *c.ByteBudget < 512 {
		return fmt.Errorf("output_firewall.byte_budget must be at least 512")
	}
	if c.MaxByteBudget != nil && *c.MaxByteBudget < 512 {
		return fmt.Errorf("output_firewall.max_byte_budget must be at least 512")
	}
	if c.ByteBudget != nil && c.MaxByteBudget != nil && *c.MaxByteBudget < *c.ByteBudget {
		return fmt.Errorf("output_firewall.max_byte_budget must be at least output_firewall.byte_budget")
	}
	if c.SpillMode != "" && c.SpillMode != "secure" && c.SpillMode != "disabled" && c.SpillMode != "required" {
		return fmt.Errorf("output_firewall.spill_mode must be secure, disabled, or required")
	}
	if c.SpillPath != "" && (filepath.IsAbs(c.SpillPath) || c.SpillPath == "." || strings.HasPrefix(filepath.Clean(c.SpillPath), "..")) {
		return fmt.Errorf("output_firewall.spill_path must be city-relative")
	}
	if c.RetentionTTL != "" {
		d, err := time.ParseDuration(c.RetentionTTL)
		if err != nil || d <= 0 {
			return fmt.Errorf("output_firewall.retention_ttl must be a positive duration")
		}
	}
	known := map[string]bool{"show": true, "ready": true, "list": true, "query": true, "mol": true, "hook": true}
	seen := map[string]bool{}
	for _, verb := range c.ReadVerbs {
		if !known[verb] {
			return fmt.Errorf("output_firewall.read_verbs: %q is not a known read route", verb)
		}
		if seen[verb] {
			return fmt.Errorf("output_firewall.read_verbs: duplicate %q", verb)
		}
		seen[verb] = true
	}
	return nil
}
