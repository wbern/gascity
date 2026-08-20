package config

import "testing"

func TestResolveContextAdvisoryPerAgentOverridesGlobal(t *testing.T) {
	builtin := DefaultContextAdvisory()
	global := &ContextAdvisory{WindowTokens: contextAdvisoryPtr(1_000_000)}
	perAgent := &ContextAdvisory{Tiers: []ContextAdvisoryTier{{
		Threshold: contextAdvisoryPtr(75),
		Message:   contextAdvisoryPtr("Agent {{.Pct}}%"),
	}}}

	policy := ResolveContextAdvisory(&builtin, global, perAgent)
	if !policy.Enabled || policy.WindowTokens != 1_000_000 {
		t.Fatalf("policy = %+v, want enabled policy with global window", policy)
	}
	if len(policy.Tiers) != 1 || policy.Tiers[0].Threshold != 75 {
		t.Fatalf("tiers = %+v, want per-agent tier", policy.Tiers)
	}
}

func TestContextAdvisorySelectTierKeepsLegacyThresholdBoundaries(t *testing.T) {
	policy := ResolveContextAdvisory(contextAdvisoryPtr(DefaultContextAdvisory()))

	if _, ok := policy.SelectTier(59.9); ok {
		t.Fatal("59.9% selected an advisory tier; legacy context injection begins at 60%")
	}
	if tier, ok := policy.SelectTier(60); !ok || tier.Threshold != 60 {
		t.Fatalf("60%% tier = %#v, %t; want the 60%% advisory tier", tier, ok)
	}
	if tier, ok := policy.SelectTier(80); !ok || tier.Threshold != 60 {
		t.Fatalf("80%% tier = %#v, %t; want the 60%% advisory tier", tier, ok)
	}
	if tier, ok := policy.SelectTier(80.1); !ok || tier.Threshold != 80 {
		t.Fatalf("80.1%% tier = %#v, %t; want the 80%% urgent tier", tier, ok)
	}
}

func TestParseContextAdvisoryAtGlobalAndAgentScope(t *testing.T) {
	cfg, err := Parse([]byte(`
[agent_defaults.context_advisory]
window_tokens = 500000
[[agent_defaults.context_advisory.tiers]]
threshold = 70
message = "global {{.Pct}}"

[[agent]]
name = "worker"
[agent.context_advisory]
enabled = false
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.AgentDefaults.ContextAdvisory == nil || cfg.AgentDefaults.ContextAdvisory.WindowTokens == nil || *cfg.AgentDefaults.ContextAdvisory.WindowTokens != 500000 {
		t.Fatalf("global advisory = %#v", cfg.AgentDefaults.ContextAdvisory)
	}
	if len(cfg.Agents) != 1 || cfg.Agents[0].ContextAdvisory == nil || cfg.Agents[0].ContextAdvisory.Enabled == nil || *cfg.Agents[0].ContextAdvisory.Enabled {
		t.Fatalf("agent advisory = %#v", cfg.Agents)
	}
}

func TestMergeAgentDefaultsContextAdvisoryPreservesUnsetFields(t *testing.T) {
	base := AgentDefaults{ContextAdvisory: &ContextAdvisory{Enabled: contextAdvisoryPtr(true), WindowTokens: contextAdvisoryPtr(1_000_000)}}
	mergeAgentDefaults(&base, AgentDefaults{ContextAdvisory: &ContextAdvisory{WindowTokens: contextAdvisoryPtr(500_000)}}, "fragment", nil)
	if base.ContextAdvisory == nil || base.ContextAdvisory.Enabled == nil || !*base.ContextAdvisory.Enabled || base.ContextAdvisory.WindowTokens == nil || *base.ContextAdvisory.WindowTokens != 500_000 {
		t.Fatalf("merged advisory = %#v", base.ContextAdvisory)
	}
}

func TestParseRejectsInvalidContextAdvisory(t *testing.T) {
	_, err := Parse([]byte(`
[agent_defaults.context_advisory]
[[agent_defaults.context_advisory.tiers]]
threshold = 101
message = "too late"
`))
	if err == nil {
		t.Fatal("Parse accepted an invalid context_advisory threshold")
	}
}
