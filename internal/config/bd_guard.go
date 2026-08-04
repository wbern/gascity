package config

import (
	"fmt"
	"strings"
)

// BdGuardConfig configures positive authorization for managed agent sessions
// to access the city bead store through gc bd. Raw bd and direct store access
// are outside this guard's boundary.
type BdGuardConfig struct {
	// Enabled fences every managed agent from the city bead store unless its
	// exact identity is listed in hq_access_agents. Unset or false preserves
	// legacy gc bd routing.
	Enabled bool `toml:"enabled,omitempty"`
	// HQAccessAgents is the hq_access_agents list of exact configured identities
	// authorized to use the city bead store, such as "worker" or
	// "my-rig/worker". An enabled config rejects unknown entries.
	HQAccessAgents []string `toml:"hq_access_agents,omitempty"`
}

// HasHQAccess reports whether the guard is enabled and the agent's exact
// configured identity is authorized to access the city bead store.
func (c BdGuardConfig) HasHQAccess(agent *Agent) bool {
	if !c.Enabled || agent == nil {
		return false
	}
	for _, identity := range c.HQAccessAgents {
		identity = strings.TrimSpace(identity)
		if AgentMatchesIdentity(agent, identity) || strings.TrimSpace(agent.PoolName) == identity {
			return true
		}
	}
	return false
}

// ValidateBdGuard rejects enabled HQ-access entries that do not identify an
// effective configured agent exactly.
func ValidateBdGuard(cfg *City) error {
	if cfg == nil || !cfg.BdGuard.Enabled {
		return nil
	}
	seen := make(map[string]struct{}, len(cfg.BdGuard.HQAccessAgents))
	for _, raw := range cfg.BdGuard.HQAccessAgents {
		identity := strings.TrimSpace(raw)
		if identity == "" {
			return fmt.Errorf("bd_guard.hq_access_agents: agent identity must not be empty")
		}
		if _, duplicate := seen[identity]; duplicate {
			return fmt.Errorf("bd_guard.hq_access_agents: duplicate agent %q", identity)
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
			return fmt.Errorf("bd_guard.hq_access_agents: agent %q is not configured", identity)
		}
	}
	return nil
}
