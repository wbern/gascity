package main

import (
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestResolveTemplateProjectsBdGuardOnlyForAllowedAgentAndFingerprintsIt(t *testing.T) {
	cityPath := t.TempDir()
	writeTemplateResolveCityConfig(t, cityPath, "file")
	allowed := &config.Agent{Name: "allowed"}
	other := &config.Agent{Name: "other"}
	city := &config.City{
		Agents: []config.Agent{*allowed, *other},
		BdGuard: config.BdGuardConfig{
			Enabled:       true,
			AllowedAgents: []string{"allowed"},
		},
	}
	params := &agentBuildParams{
		cityName:   "city",
		cityPath:   cityPath,
		city:       city,
		workspace:  &config.Workspace{Provider: "test"},
		providers:  map[string]config.ProviderSpec{"test": {Command: "echo", PromptMode: "none"}},
		lookPath:   func(string) (string, error) { return "/bin/echo", nil },
		fs:         fsys.OSFS{},
		beaconTime: time.Unix(0, 0),
		beadNames:  make(map[string]string),
		stderr:     io.Discard,
	}

	guarded, err := resolveTemplate(params, allowed, allowed.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate(allowed): %v", err)
	}
	if got := guarded.Env[bdGuardMarkerEnv]; got != bdGuardMarkerValue {
		t.Fatalf("%s = %q, want %q", bdGuardMarkerEnv, got, bdGuardMarkerValue)
	}
	if got := guarded.Env[bdGuardCityEnv]; got != cityPath {
		t.Fatalf("%s = %q, want %q", bdGuardCityEnv, got, cityPath)
	}

	unguardedOther, err := resolveTemplate(params, other, other.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate(other): %v", err)
	}
	if got := unguardedOther.Env[bdGuardMarkerEnv]; got != "" {
		t.Fatalf("%s = %q for non-allowlisted agent, want explicit empty scrub marker", bdGuardMarkerEnv, got)
	}

	city.BdGuard.Enabled = false
	unguardedSameAgent, err := resolveTemplate(params, allowed, allowed.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate(allowed, guard off): %v", err)
	}
	if runtime.CoreFingerprint(templateParamsToConfig(guarded)) == runtime.CoreFingerprint(templateParamsToConfig(unguardedSameAgent)) {
		t.Fatal("guard projection did not change the managed-session core fingerprint")
	}
}
