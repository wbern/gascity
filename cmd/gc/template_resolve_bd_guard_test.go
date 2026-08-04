package main

import (
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestResolveTemplateProjectsBdGuardWithPositiveHQAuthorization(t *testing.T) {
	cityPath := t.TempDir()
	writeTemplateResolveCityConfig(t, cityPath, "file")
	authorized := &config.Agent{Name: "authorized"}
	other := &config.Agent{Name: "other"}
	city := &config.City{
		Agents: []config.Agent{*authorized, *other},
		BdGuard: config.BdGuardConfig{
			Enabled:        true,
			HQAccessAgents: []string{"authorized"},
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

	authorizedSession, err := resolveTemplate(params, authorized, authorized.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate(authorized): %v", err)
	}
	if got := authorizedSession.Env[bdGuardMarkerEnv]; got != bdGuardMarkerValue {
		t.Fatalf("%s = %q, want %q", bdGuardMarkerEnv, got, bdGuardMarkerValue)
	}
	if got := authorizedSession.Env[bdGuardAccessEnv]; got != bdGuardMarkerValue {
		t.Fatalf("%s = %q, want %q", bdGuardAccessEnv, got, bdGuardMarkerValue)
	}
	if got := authorizedSession.Env[bdGuardCityEnv]; got != cityPath {
		t.Fatalf("%s = %q, want %q", bdGuardCityEnv, got, cityPath)
	}

	fencedOther, err := resolveTemplate(params, other, other.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate(other): %v", err)
	}
	if got := fencedOther.Env[bdGuardMarkerEnv]; got != bdGuardMarkerValue {
		t.Fatalf("%s = %q for unlisted agent, want active guard marker", bdGuardMarkerEnv, got)
	}
	if got := fencedOther.Env[bdGuardAccessEnv]; got != "" {
		t.Fatalf("%s = %q for unlisted agent, want explicit empty authorization", bdGuardAccessEnv, got)
	}

	city.BdGuard.Enabled = false
	legacySession, err := resolveTemplate(params, authorized, authorized.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate(authorized, guard off): %v", err)
	}
	if got := legacySession.Env[bdGuardMarkerEnv]; got != "" {
		t.Fatalf("%s = %q with guard disabled, want explicit empty scrub marker", bdGuardMarkerEnv, got)
	}
	if got := legacySession.Env[bdGuardAccessEnv]; got != "" {
		t.Fatalf("%s = %q with guard disabled, want explicit empty scrub authorization", bdGuardAccessEnv, got)
	}
	if runtime.CoreFingerprint(templateParamsToConfig(authorizedSession)) == runtime.CoreFingerprint(templateParamsToConfig(legacySession)) {
		t.Fatal("guard projection did not change the managed-session core fingerprint")
	}
	if runtime.CoreFingerprint(templateParamsToConfig(authorizedSession)) == runtime.CoreFingerprint(templateParamsToConfig(fencedOther)) {
		t.Fatal("HQ authorization did not change the managed-session core fingerprint")
	}
}
