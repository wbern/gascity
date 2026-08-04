package config

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestLoadWithIncludesBdGuardLastCityLayerReplacesHQAccessAgents(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "city.toml", `
include = ["guard.toml"]

[[agent]]
name = "first"

[[agent]]
name = "second"

[bd_guard]
enabled = true
hq_access_agents = ["first"]
`)
	writeTestFile(t, dir, "guard.toml", `
[bd_guard]
enabled = true
hq_access_agents = ["second"]
`)

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if !cfg.BdGuard.Enabled {
		t.Fatal("BdGuard.Enabled = false, want true")
	}
	if got, want := strings.Join(cfg.BdGuard.HQAccessAgents, ","), "second"; got != want {
		t.Fatalf("BdGuard.HQAccessAgents = %q, want %q", got, want)
	}
}

func TestLoadWithIncludesBdGuardRejectsUnknownExactAgent(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "city.toml", `
[[agent]]
name = "worker"

[bd_guard]
enabled = true
hq_access_agents = ["other/worker"]
`)

	_, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(dir, "city.toml"))
	if err == nil || !strings.Contains(err.Error(), `bd_guard.hq_access_agents: agent "other/worker" is not configured`) {
		t.Fatalf("LoadWithIncludes error = %v, want exact-agent validation error", err)
	}
}

func TestValidateBdGuardRejectsInvalidHQAccessEntries(t *testing.T) {
	cases := []struct {
		name    string
		entries []string
		want    string
	}{
		{"empty", []string{" "}, "agent identity must not be empty"},
		{"duplicate after trimming", []string{"worker", " worker "}, `duplicate agent "worker"`},
		{"unknown", []string{"other"}, `agent "other" is not configured`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &City{
				Agents:  []Agent{{Name: "worker"}},
				BdGuard: BdGuardConfig{Enabled: true, HQAccessAgents: tc.entries},
			}
			err := ValidateBdGuard(cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ValidateBdGuard() error = %v, want %q", err, tc.want)
			}
		})
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
hq_access_agents = ["worker"]
`)

	_, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(dir, "city.toml"))
	if err == nil || !strings.Contains(err.Error(), "bd_guard") {
		t.Fatalf("LoadWithIncludes error = %v, want pack authoring rejection for bd_guard", err)
	}
}

func TestBdGuardAbsentAndDisabledPreserveLegacyBehavior(t *testing.T) {
	for _, source := range []string{
		"[workspace]\nname = \"demo\"\n",
		"[workspace]\nname = \"demo\"\n[bd_guard]\nenabled = false\nhq_access_agents = [\"not-configured\"]\n",
	} {
		cfg, err := Parse([]byte(source))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if cfg.BdGuard.HasHQAccess(&Agent{Name: "anything"}) {
			t.Fatalf("BdGuard.HasHQAccess = true for config:\n%s", source)
		}
		if err := ValidateBdGuard(cfg); err != nil {
			t.Fatalf("ValidateBdGuard disabled config: %v", err)
		}
	}
}

func TestBdGuardGrantsHQAccessToPoolInstancesByExactTemplateIdentity(t *testing.T) {
	guard := BdGuardConfig{Enabled: true, HQAccessAgents: []string{"rig/worker"}}
	instance := &Agent{Name: "worker-3", Dir: "rig", PoolName: "rig/worker"}
	if !guard.HasHQAccess(instance) {
		t.Fatal("BdGuard.HasHQAccess(pool instance) = false, want template access entry to cover its instances")
	}
}
