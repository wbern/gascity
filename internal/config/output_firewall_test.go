package config

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestValidateOutputFirewallRejectsUnsafePolicy(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  OutputFirewallConfig
	}{
		{"small budget", OutputFirewallConfig{ByteBudget: intPtr(1)}},
		{"small maximum budget", OutputFirewallConfig{MaxByteBudget: intPtr(1)}},
		{"maximum below city budget", OutputFirewallConfig{ByteBudget: intPtr(1024), MaxByteBudget: intPtr(512)}},
		{"mutation verb", OutputFirewallConfig{ReadVerbs: []string{"update"}}},
		{"absolute spill", OutputFirewallConfig{SpillPath: "/tmp/output"}},
		{"bad retention", OutputFirewallConfig{RetentionTTL: "0s"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateOutputFirewall(&City{OutputFirewall: tc.cfg}); err == nil {
				t.Fatal("ValidateOutputFirewall() succeeded")
			}
		})
	}
}

func TestEffectiveAgentByteBudget(t *testing.T) {
	if got := (OutputFirewallConfig{}).EffectiveAgentByteBudget(nil); got != 32768 {
		t.Fatalf("default budget = %d, want 32768", got)
	}
	if got := (OutputFirewallConfig{ByteBudget: intPtr(8192)}).EffectiveAgentByteBudget(nil); got != 8192 {
		t.Fatalf("city fallback budget = %d, want 8192", got)
	}
	if got := (OutputFirewallConfig{}).EffectiveAgentByteBudget(intPtr(65536)); got != 32768 {
		t.Fatalf("default city ceiling = %d, want 32768", got)
	}
	if got := (OutputFirewallConfig{ByteBudget: intPtr(8192), MaxByteBudget: intPtr(16384)}).EffectiveAgentByteBudget(intPtr(65536)); got != 16384 {
		t.Fatalf("clamped agent budget = %d, want 16384", got)
	}
}

func TestValidateAgentsRejectsUnsafeOutputFirewallBudget(t *testing.T) {
	err := ValidateAgents([]Agent{{Name: "runner", OutputFirewallByteBudget: intPtr(1)}})
	if err == nil || !strings.Contains(err.Error(), "output_firewall_byte_budget") {
		t.Fatalf("ValidateAgents() error = %v, want output firewall budget rejection", err)
	}
}

func TestLoadWithIncludesOutputFirewallCannotBeAuthoredByPack(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "city.toml", "[workspace]\nname = \"demo\"\n")
	writeTestFile(t, dir, "pack.toml", `
[pack]
schema = 2
name = "untrusted"

[output_firewall]
byte_budget = 512
`)
	_, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(dir, "city.toml"))
	if err == nil || !strings.Contains(err.Error(), "output_firewall") {
		t.Fatalf("LoadWithIncludes error = %v, want pack authoring rejection", err)
	}
}

func TestValidateOutputFirewallAcceptsKnownReadPolicy(t *testing.T) {
	cfg := OutputFirewallConfig{ByteBudget: intPtr(32768), ReadVerbs: []string{"show", "ready", "list"}, SpillMode: "secure", SpillPath: ".gc/evidence/output", RetentionTTL: "24h"}
	if err := ValidateOutputFirewall(&City{OutputFirewall: cfg}); err != nil {
		t.Fatalf("ValidateOutputFirewall() = %v", err)
	}
}

func TestLoadWithIncludesFragmentOverridesOutputFirewall(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["firewall.toml"]

[workspace]
name = "test"

[output_firewall]
byte_budget = 4096
`)
	fs.Files["/city/firewall.toml"] = []byte(`
[output_firewall]
byte_budget = 8192
spill_mode = "disabled"
`)

	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if got := cfg.OutputFirewall.EffectiveByteBudget(); got != 8192 {
		t.Fatalf("EffectiveByteBudget = %d, want fragment override 8192", got)
	}
	if got := cfg.OutputFirewall.EffectiveSpillMode(); got != "disabled" {
		t.Fatalf("EffectiveSpillMode = %q, want disabled", got)
	}
}

func intPtr(v int) *int { return &v }
