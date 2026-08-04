package config

import (
	"fmt"
	"strings"
)

// BdGuardConfig configures the opt-in gc bd city-store fence projected into
// selected managed agent sessions. Raw bd and direct store access are outside
// this guard's boundary.
type BdGuardConfig struct {
	// Enabled activates projection for exact identities in AllowedAgents.
	// Unset or false preserves legacy gc bd routing.
	Enabled bool `toml:"enabled,omitempty"`
	// AllowedAgents lists exact configured agent identities to fence, such as
	// "worker" or "my-rig/worker". An enabled config rejects unknown entries.
	AllowedAgents []string `toml:"allowed_agents,omitempty"`
}

// AppliesTo reports whether the guard is enabled for the exact configured
// identity of agent.
func (c BdGuardConfig) AppliesTo(agent *Agent) bool {
	if !c.Enabled || agent == nil {
		return false
	}
	for _, identity := range c.AllowedAgents {
		identity = strings.TrimSpace(identity)
		if AgentMatchesIdentity(agent, identity) || strings.TrimSpace(agent.PoolName) == identity {
			return true
		}
	}
	return false
}

// ValidateBdGuard rejects enabled allowlist entries that do not identify an
// effective configured agent exactly.
func ValidateBdGuard(cfg *City) error {
	if cfg == nil || !cfg.BdGuard.Enabled {
		return nil
	}
	seen := make(map[string]struct{}, len(cfg.BdGuard.AllowedAgents))
	for _, raw := range cfg.BdGuard.AllowedAgents {
		identity := strings.TrimSpace(raw)
		if identity == "" {
			return fmt.Errorf("bd_guard.allowed_agents: agent identity must not be empty")
		}
		if _, duplicate := seen[identity]; duplicate {
			return fmt.Errorf("bd_guard.allowed_agents: duplicate agent %q", identity)
		}
		seen[identity] = struct{}{}
		found := false
		for i := range cfg.Agents {
			if AgentMatchesIdentity(&cfg.Agents[i], identity) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("bd_guard.allowed_agents: agent %q is not configured", identity)
		}
	}
	return nil
}
