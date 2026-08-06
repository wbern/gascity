package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/overlay"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestBuildDesiredState_MergeableHookWithoutInstallHooksDoesNotDrift pins
// gcw-u67z end to end, in the production shape.
//
// An agent whose pack overlay ships a mergeable hook file, but which does NOT
// declare install_agent_hooks for that provider slot, hits a gap between two
// stagers: materializeProviderOverlaysBeforeFingerprint skips mergeable paths
// (so hooks.Install can be their sole writer) and hooks.Install only iterates
// install_agent_hooks — so nothing creates the file before the fingerprint is
// taken. Session start then stages it with no skip. The CopyEntry goes absent
// -> present, the CopyFiles field hash moves, and the reconciler reads config
// drift and restarts the session it just started.
//
// This is distinct from gcw-0cv5, which fixed what an entry HASHES. This one is
// about whether the entry EXISTS.
func TestBuildDesiredState_MergeableHookWithoutInstallHooksDoesNotDrift(t *testing.T) {
	for _, tc := range []struct {
		provider string
		relPath  string
	}{
		{"codex", filepath.Join(".codex", "hooks.json")},
		{"gemini", filepath.Join(".gemini", "settings.json")},
		{"antigravity", filepath.Join(".agents", "hooks.json")},
		{"cursor", filepath.Join(".cursor", "hooks.json")},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			if !overlay.IsMergeablePath(tc.relPath) {
				t.Fatalf("fixture broken: %q is not a mergeable path", tc.relPath)
			}
			cityPath := resolvedTempDir(t)
			packOverlay := filepath.Join(cityPath, "packs", "core", "overlay")
			overlayHook := filepath.Join(packOverlay, overlay.PerProviderDir, tc.provider, tc.relPath)
			if err := os.MkdirAll(filepath.Dir(overlayHook), 0o755); err != nil {
				t.Fatalf("MkdirAll: %v", err)
			}
			if err := os.WriteFile(overlayHook, []byte(`{"hooks":{"SessionStart":[]}}`+"\n"), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			workDir := filepath.Join(cityPath, "worker")
			if err := os.MkdirAll(workDir, 0o755); err != nil {
				t.Fatalf("MkdirAll: %v", err)
			}

			base := "builtin:" + tc.provider
			cfg := &config.City{
				Workspace: config.Workspace{Name: "test-city", Provider: tc.provider},
				Providers: map[string]config.ProviderSpec{
					tc.provider: {
						Base:          &base,
						Command:       "/bin/echo",
						ResumeCommand: "/bin/echo resume {{.SessionKey}}",
					},
				},
				PackOverlayDirs: []string{packOverlay},
				Agents: []config.Agent{{
					Name:              "probe",
					MaxActiveSessions: intPtr(1),
					ScaleCheck:        "echo 1",
					WorkDir:           "worker",
					// Deliberately NO InstallAgentHooks — the whole point.
				}},
			}

			fingerprintNow := func(cycle int) string {
				ds := buildDesiredState("test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(), nil, io.Discard)
				if len(ds.State) != 1 {
					t.Fatalf("cycle %d: desired state size %d, want 1", cycle, len(ds.State))
				}
				var tp TemplateParams
				for _, v := range ds.State {
					tp = v
				}
				rcfg := templateParamsToConfig(tp)
				if rcfg.WorkDir == "" {
					t.Fatalf("cycle %d: missing runtime config", cycle)
				}
				fp := runtime.CoreFingerprint(rcfg)
				if err := runtime.StageSessionWorkDir(rcfg); err != nil {
					t.Fatalf("cycle %d: StageSessionWorkDir: %v", cycle, err)
				}
				return fp
			}

			first := fingerprintNow(1)
			second := fingerprintNow(2)
			if first != second {
				t.Fatalf("core fingerprint moved across the first staging (entry-presence drift):\n first=%s\n second=%s", first, second)
			}
			if third := fingerprintNow(3); third != second {
				t.Fatalf("core fingerprint unstable on a later cycle: second=%s third=%s", second, third)
			}
		})
	}
}
