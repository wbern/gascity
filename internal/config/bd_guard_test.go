package config

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestLoadWithIncludesBdGuardLastCityLayerReplacesAllowlist(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "city.toml", `
include = ["guard.toml"]

[[agent]]
name = "first"

[[agent]]
name = "second"

[bd_guard]
enabled = true
allowed_agents = ["first"]
`)
	writeTestFile(t, dir, "guard.toml", `
[bd_guard]
enabled = true
allowed_agents = ["second"]
`)

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if !cfg.BdGuard.Enabled {
		t.Fatal("BdGuard.Enabled = false, want true")
	}
	if got, want := strings.Join(cfg.BdGuard.AllowedAgents, ","), "second"; got != want {
		t.Fatalf("BdGuard.AllowedAgents = %q, want %q", got, want)
	}
}

func TestLoadWithIncludesBdGuardRejectsUnknownExactAgent(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "city.toml", `
[[agent]]
name = "worker"

[bd_guard]
enabled = true
allowed_agents = ["other/worker"]
`)

	_, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(dir, "city.toml"))
	if err == nil || !strings.Contains(err.Error(), `bd_guard.allowed_agents: agent "other/worker" is not configured`) {
		t.Fatalf("LoadWithIncludes error = %v, want exact-agent validation error", err)
	}
}

func TestLoadWithIncludesBdGuardCannotBeEnabledByPack(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "city.toml", "[workspace]\nname = \"demo\"\n")
	writeTestFile(t, dir, "pack.toml", `
[pack]
schema = 2
name = "untrusted"

[bd_guard]
enabled = true
allowed_agents = ["worker"]
`)

	_, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(dir, "city.toml"))
	if err == nil || !strings.Contains(err.Error(), "bd_guard") {
		t.Fatalf("LoadWithIncludes error = %v, want pack authoring rejection for bd_guard", err)
	}
}

func TestBdGuardAbsentAndDisabledPreserveLegacyBehavior(t *testing.T) {
	for _, source := range []string{
		"[workspace]\nname = \"demo\"\n",
		"[workspace]\nname = \"demo\"\n[bd_guard]\nenabled = false\nallowed_agents = [\"not-configured\"]\n",
	} {
		cfg, err := Parse([]byte(source))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if cfg.BdGuard.AppliesTo(&Agent{Name: "anything"}) {
			t.Fatalf("BdGuard.AppliesTo = true for config:\n%s", source)
		}
		if err := ValidateBdGuard(cfg); err != nil {
			t.Fatalf("ValidateBdGuard disabled config: %v", err)
		}
	}
}

func TestBdGuardAppliesToPoolInstancesByExactTemplateIdentity(t *testing.T) {
	guard := BdGuardConfig{Enabled: true, AllowedAgents: []string{"rig/worker"}}
	instance := &Agent{Name: "worker-3", Dir: "rig", PoolName: "rig/worker"}
	if !guard.AppliesTo(instance) {
		t.Fatal("BdGuard.AppliesTo(pool instance) = false, want template allowlist to cover its instances")
	}
}
