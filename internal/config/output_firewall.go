package config

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// OutputFirewallConfig is the operator-owned policy applied to managed read output.
type OutputFirewallConfig struct {
	Enabled      *bool    `toml:"enabled,omitempty"`
	ByteBudget   *int     `toml:"byte_budget,omitempty"`
	ReadVerbs    []string `toml:"read_verbs,omitempty"`
	SpillMode    string   `toml:"spill_mode,omitempty"`
	SpillPath    string   `toml:"spill_path,omitempty"`
	RetentionTTL string   `toml:"retention_ttl,omitempty"`
}

// EffectiveByteBudget returns the managed-output budget.
func (c OutputFirewallConfig) EffectiveByteBudget() int {
	if c.ByteBudget == nil {
		return 32 << 10
	}
	return *c.ByteBudget
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
