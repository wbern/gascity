package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/bootstrap/packs/core"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/hooks"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// seedCodexOverlay writes the real embedded core codex hooks overlay into a
// temp overlay source dir (per-provider/codex/.codex/hooks.json) so staging and
// hooks.Install operate on the same bytes the reconciler uses in production.
func seedCodexOverlay(t *testing.T) string {
	t.Helper()
	data, err := core.PackFS.ReadFile("overlay/per-provider/codex/.codex/hooks.json")
	if err != nil {
		t.Fatalf("read embedded codex hooks overlay: %v", err)
	}
	src := t.TempDir()
	dstDir := filepath.Join(src, "per-provider", "codex", ".codex")
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		t.Fatalf("mkdir codex overlay: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dstDir, "hooks.json"), data, 0o644); err != nil {
		t.Fatalf("write codex overlay: %v", err)
	}
	return src
}

// codexSessionStartMatchers returns the "matcher" value of every SessionStart
// hook entry in a codex hooks.json document.
func codexSessionStartMatchers(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	matchers := make([]string, 0, len(doc.Hooks["SessionStart"]))
	for _, e := range doc.Hooks["SessionStart"] {
		matchers = append(matchers, e.Matcher)
	}
	return matchers
}

// furiosaHybridCodexHooks is the live hybrid captured from the gc2 furiosa
// polecat (see gcw-zd0v / gcw-mnck): the reconciler's bound `matcher:"startup"`
// SessionStart entry coexisting with the overlay's pre-#3866 unbound
// `matcher:""` `gc prime` entry, plus the unbound PreCompact/UserPromptSubmit
// entries. `gc doctor` flags this as codex-hooks-drift ("needs upgrade")
// forever because the two writers keep re-seeding disagreeing matchers.
const furiosaHybridCodexHooks = `{
  "hooks": {
    "PreCompact": [
      {
        "hooks": [
          {
            "command": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gc handoff --auto --hook-format codex \"context cycle\"",
            "type": "command"
          }
        ],
        "matcher": ""
      }
    ],
    "SessionStart": [
      {
        "hooks": [
          {
            "command": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && GC_MANAGED_SESSION_HOOK=1 GC_HOOK_EVENT_NAME=SessionStart gc --city '__CITY__' prime --hook --hook-format codex",
            "type": "command"
          }
        ],
        "matcher": "startup"
      },
      {
        "hooks": [
          {
            "command": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && GC_MANAGED_SESSION_HOOK=1 GC_HOOK_EVENT_NAME=SessionStart gc prime --hook --hook-format codex",
            "type": "command"
          }
        ],
        "matcher": ""
      }
    ],
    "UserPromptSubmit": [
      {
        "hooks": [
          {
            "command": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gc hook run --timeout 15s --timeout-exit-code 0 -- nudge drain --inject --hook-format codex",
            "type": "command"
          },
          {
            "command": "export PATH=\"$HOME/go/bin:$HOME/.local/bin:$PATH\" && gc hook run --timeout 15s --timeout-exit-code 0 -- mail check --inject --hook-format codex",
            "type": "command"
          }
        ],
        "matcher": ""
      }
    ],
    "Stop": [
      {
        "hooks": [
          {
            "command": "echo user-authored-hook",
            "type": "command"
          }
        ],
        "matcher": ""
      }
    ]
  }
}`

// seedFuriosaHybrid writes the live hybrid fixture into workDir/.codex/hooks.json
// with its bound SessionStart entry pinned to cityDir, reproducing the drifted
// starting state a reconcile tick must converge.
func seedFuriosaHybrid(t *testing.T, cityDir, workDir string) {
	t.Helper()
	dir := filepath.Join(workDir, ".codex")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir .codex: %v", err)
	}
	body := strings.ReplaceAll(furiosaHybridCodexHooks, "__CITY__", cityDir)
	if err := os.WriteFile(filepath.Join(dir, "hooks.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("seed furiosa hybrid: %v", err)
	}
}

// stageCodex runs the staging half of the build_desired_state home-dir tick.
// skipMergeable selects the fixed (skip) vs legacy (no-skip) path.
func stageCodex(t *testing.T, overlaySrc, workDir string, skipMergeable bool) {
	t.Helper()
	var err error
	if skipMergeable {
		err = runtime.StageProviderOverlayDirSkippingMergeable(overlaySrc, workDir, []string{"codex"}, nil)
	} else {
		err = runtime.StageProviderOverlayDir(overlaySrc, workDir, []string{"codex"}, nil)
	}
	if err != nil {
		t.Fatalf("stage codex overlay (skip=%v): %v", skipMergeable, err)
	}
}

// installCodex runs the hooks.Install half of the tick on the same workDir.
func installCodex(t *testing.T, cityDir, workDir string) {
	t.Helper()
	if err := hooks.Install(fsys.OSFS{}, cityDir, workDir, []string{"codex"}); err != nil {
		t.Fatalf("hooks.Install codex: %v", err)
	}
}

// TestCodexHooksConvergeWithSkipStaging is the gcw-mnck reproduce+fix test.
//
// The build_desired_state home-dir tick is staging followed by hooks.Install on
// the SAME dir. Starting from the live furiosa hybrid, hooks.Install converges
// the document to a single bound SessionStart entry — but the LEGACY staging
// path re-merges the overlay's unbound `matcher:""` entry back in on the very
// next tick, so the on-disk document a fresh `gc doctor`/session-start reads
// right after staging is perpetually drifted ([startup, ""]). That oscillation
// is why codex-hooks-drift never clears without --fix.
//
// The observation point that matters is therefore the post-staging state. With
// the skip path staging no longer touches the mergeable file, so the converged
// [startup] document is stable at every point in the cycle.
func TestCodexHooksConvergeWithSkipStaging(t *testing.T) {
	overlaySrc := seedCodexOverlay(t)
	cityDir := t.TempDir()

	assertSingleBound := func(t *testing.T, workDir, when string) {
		t.Helper()
		hooksPath := filepath.Join(workDir, ".codex", "hooks.json")
		matchers := codexSessionStartMatchers(t, hooksPath)
		data, _ := os.ReadFile(hooksPath)
		if len(matchers) != 1 || matchers[0] != "startup" {
			t.Fatalf("%s: SessionStart matchers = %v, want exactly [startup] (converged, bound)\n%s", when, matchers, data)
		}
		if !strings.Contains(string(data), "--city") {
			t.Fatalf("%s: converged SessionStart not bound to city root (missing --city)\n%s", when, data)
		}
		if strings.Contains(string(data), "gc prime --hook") {
			t.Fatalf("%s: unbound `gc prime` SessionStart entry still present (drift)\n%s", when, data)
		}
	}

	// assertManagedEventsIntact guards the gcw-mnck regression surface. Because
	// the home-dir staging path now skips the ENTIRE .codex/hooks.json, hooks.Install
	// must remain the sole, COMPLETE writer: the converged document has to keep the
	// managed PreCompact (context-cycle handoff) and UserPromptSubmit (mail check +
	// nudge drain) hooks, not just SessionStart. A future change to the installer's
	// fresh-write/upgrade path that dropped either event would otherwise slip past
	// assertSingleBound, which only inspects SessionStart.
	assertManagedEventsIntact := func(t *testing.T, workDir, when string) {
		t.Helper()
		hooksPath := filepath.Join(workDir, ".codex", "hooks.json")
		data, err := os.ReadFile(hooksPath)
		if err != nil {
			t.Fatalf("%s: read %s: %v", when, hooksPath, err)
		}
		var doc struct {
			Hooks map[string][]struct {
				Hooks []struct {
					Command string `json:"command"`
				} `json:"hooks"`
			} `json:"hooks"`
		}
		if err := json.Unmarshal(data, &doc); err != nil {
			t.Fatalf("%s: unmarshal %s: %v", when, hooksPath, err)
		}
		commandsFor := func(event string) string {
			var b strings.Builder
			for _, e := range doc.Hooks[event] {
				for _, h := range e.Hooks {
					b.WriteString(h.Command)
					b.WriteByte('\n')
				}
			}
			return b.String()
		}
		if !strings.Contains(commandsFor("PreCompact"), "handoff") {
			t.Fatalf("%s: converged doc dropped the managed PreCompact handoff hook\n%s", when, data)
		}
		prompt := commandsFor("UserPromptSubmit")
		if !strings.Contains(prompt, "mail check") || !strings.Contains(prompt, "nudge drain") {
			t.Fatalf("%s: converged doc dropped managed UserPromptSubmit hooks (want mail check + nudge drain)\n%s", when, data)
		}
	}

	// Fixed path: seed the drifted hybrid, then run stage → install → stage.
	// The file must be converged and bound at EVERY observation point, including
	// the post-staging states where the legacy path re-drifts.
	fixedWork := t.TempDir()
	seedFuriosaHybrid(t, cityDir, fixedWork)
	stageCodex(t, overlaySrc, fixedWork, true)
	installCodex(t, cityDir, fixedWork)
	assertSingleBound(t, fixedWork, "fixed after install")
	assertManagedEventsIntact(t, fixedWork, "fixed after install")
	stageCodex(t, overlaySrc, fixedWork, true)
	assertSingleBound(t, fixedWork, "fixed after re-stage")
	assertManagedEventsIntact(t, fixedWork, "fixed after re-stage")

	// Legacy path: identical sequence with non-skip staging. After the trailing
	// staging step the unbound overlay entry is merged back in, re-creating the
	// hybrid a reconcile tick can never settle. This is the drift the fix removes.
	legacyWork := t.TempDir()
	seedFuriosaHybrid(t, cityDir, legacyWork)
	stageCodex(t, overlaySrc, legacyWork, false)
	installCodex(t, cityDir, legacyWork)
	stageCodex(t, overlaySrc, legacyWork, false)
	legacyMatchers := codexSessionStartMatchers(t, filepath.Join(legacyWork, ".codex", "hooks.json"))
	if len(legacyMatchers) <= 1 {
		t.Fatalf("expected legacy non-skip staging to re-drift the hybrid (>1 SessionStart entry) after re-staging, got %v; if this no longer reproduces, the dual-write may have been fixed elsewhere — re-verify the skip is still required", legacyMatchers)
	}
}

// TestStageSessionWorkDirConvergesManagedCodexHooks covers the overlap that
// remains after desired-state materialization: a configured persistent home is
// also the runtime workdir. Runtime staging must not reintroduce the overlay's
// unbound SessionStart entry after hooks.Install already converged the file.
func TestStageSessionWorkDirConvergesManagedCodexHooks(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	overlaySrc := seedCodexOverlay(t)
	cityDir := t.TempDir()
	workDir := t.TempDir()
	seedFuriosaHybrid(t, cityDir, workDir)
	installCodex(t, cityDir, workDir)

	cfg := runtime.Config{
		WorkDir:           workDir,
		ProviderName:      "codex",
		InstallAgentHooks: []string{"codex"},
		PackOverlayDirs:   []string{overlaySrc},
	}
	configureManagedHookConvergence(&cfg, cityDir)
	if err := runtime.StageSessionWorkDir(cfg); err != nil {
		t.Fatalf("StageSessionWorkDir: %v", err)
	}

	hooksPath := filepath.Join(workDir, ".codex", "hooks.json")
	matchers := codexSessionStartMatchers(t, hooksPath)
	if len(matchers) != 1 || matchers[0] != "startup" {
		data, _ := os.ReadFile(hooksPath)
		t.Fatalf("SessionStart matchers = %v, want exactly [startup] after runtime staging\n%s", matchers, data)
	}
	data, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatalf("read staged hooks: %v", err)
	}
	if !strings.Contains(string(data), "echo user-authored-hook") {
		t.Fatalf("runtime hook convergence removed user-authored hook:\n%s", data)
	}
	first, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatalf("read first staged hooks: %v", err)
	}
	if err := runtime.StageSessionWorkDir(cfg); err != nil {
		t.Fatalf("StageSessionWorkDir second pass: %v", err)
	}
	second, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatalf("read second staged hooks: %v", err)
	}
	if string(first) != string(second) {
		t.Fatalf("repeated runtime staging changed hooks.json\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

// TestStageSessionWorkDirComposesOverlayOnlyCustomCodexHooks proves the
// normal-start path does not rely on a pre-existing managed document. Tmux
// stages configured overlays before its final convergence callback, so a fresh
// session workdir with only a custom overlay hook must end with that hook plus
// exactly one city-bound managed behavior set.
func TestStageSessionWorkDirComposesOverlayOnlyCustomCodexHooks(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	cityDir := t.TempDir()
	workDir := t.TempDir()
	overlayDir := t.TempDir()
	overlayHooks := filepath.Join(overlayDir, "per-provider", "codex", ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(overlayHooks), 0o755); err != nil {
		t.Fatalf("MkdirAll overlay hooks: %v", err)
	}
	if err := os.WriteFile(overlayHooks, []byte(`{"hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command","command":"bd overlay-custom-hook"}]}]}}`), 0o644); err != nil {
		t.Fatalf("write overlay hooks: %v", err)
	}

	cfg := runtime.Config{
		WorkDir:         workDir,
		ProviderName:    "codex",
		PackOverlayDirs: []string{overlayDir},
	}
	configureManagedHookConvergence(&cfg, cityDir)
	if err := runtime.StageSessionWorkDir(cfg); err != nil {
		t.Fatalf("StageSessionWorkDir: %v", err)
	}

	hookPath := filepath.Join(workDir, ".codex", "hooks.json")
	first, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("read converged hooks: %v", err)
	}
	if !strings.Contains(string(first), "bd overlay-custom-hook") {
		t.Fatalf("overlay-only custom hook was lost:\n%s", first)
	}
	if !hooks.CodexHooksAreConverged(first, cityDir) {
		t.Fatalf("fresh overlay-only document is not exact-one converged:\n%s", first)
	}
	if err := runtime.StageSessionWorkDir(cfg); err != nil {
		t.Fatalf("second StageSessionWorkDir: %v", err)
	}
	second, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("read second converged hooks: %v", err)
	}
	if string(first) != string(second) {
		t.Fatalf("fresh overlay-only convergence was not byte-stable:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

func TestStageSessionWorkDirConvergesCanonicalOwnerAndStripsLinkedManagedHooks(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	mainWorktree := filepath.Join(t.TempDir(), "main")
	if err := os.MkdirAll(mainWorktree, 0o755); err != nil {
		t.Fatalf("mkdir main worktree: %v", err)
	}
	runGit(t, mainWorktree, "init", "--initial-branch=main")
	runGit(t, mainWorktree, "config", "user.email", "test@example.com")
	runGit(t, mainWorktree, "config", "user.name", "Gas City Test")
	runGit(t, mainWorktree, "commit", "--allow-empty", "-m", "init")
	linkedWorktree := filepath.Join(t.TempDir(), "linked")
	runGit(t, mainWorktree, "worktree", "add", "-b", "linked", linkedWorktree)

	cityDir := t.TempDir()
	overlaySrc := seedCodexOverlay(t)
	seedFuriosaHybrid(t, cityDir, mainWorktree)
	seedFuriosaHybrid(t, cityDir, linkedWorktree)
	canonicalHooks := filepath.Join(mainWorktree, ".codex", "hooks.json")
	cfg := runtime.Config{
		WorkDir:           linkedWorktree,
		ProviderName:      "codex",
		InstallAgentHooks: []string{"codex"},
		PackOverlayDirs:   []string{overlaySrc},
	}
	configureManagedHookConvergence(&cfg, cityDir)
	if err := runtime.StageSessionWorkDir(cfg); err != nil {
		t.Fatalf("StageSessionWorkDir: %v", err)
	}

	matchers := codexSessionStartMatchers(t, canonicalHooks)
	if len(matchers) != 1 || matchers[0] != "startup" {
		data, _ := os.ReadFile(canonicalHooks)
		t.Fatalf("canonical SessionStart matchers = %v, want exactly [startup] after staging\n%s", matchers, data)
	}
	canonicalData, err := os.ReadFile(canonicalHooks)
	if err != nil {
		t.Fatalf("read canonical hooks: %v", err)
	}
	for _, command := range []string{"echo user-authored-hook", "handoff", "mail check", "nudge drain"} {
		if !strings.Contains(string(canonicalData), command) {
			t.Fatalf("canonical owner hooks dropped %q:\n%s", command, canonicalData)
		}
	}
	if !hooks.CodexHooksAreConverged(canonicalData, cityDir) {
		t.Fatalf("canonical owner is not exact-one/current-city converged:\n%s", canonicalData)
	}

	linkedHooks := filepath.Join(linkedWorktree, ".codex", "hooks.json")
	linkedData, err := os.ReadFile(linkedHooks)
	if err != nil {
		t.Fatalf("read linked hooks: %v", err)
	}
	if !strings.Contains(string(linkedData), "echo user-authored-hook") {
		t.Fatalf("linked non-owner lost custom hook:\n%s", linkedData)
	}
	linkedAudit, err := hooks.AuditCodexHooks(linkedData)
	if err != nil {
		t.Fatalf("audit linked non-owner: %v", err)
	}
	if len(linkedAudit.ManagedBehaviorCounts) == 0 {
		t.Fatalf("fixture no longer retains the inert unbound legacy handlers needed to prove conservative cleanup:\n%s", linkedData)
	}
	if _, changed, err := hooks.RemoveManagedCodexHooksForCity(linkedData, cityDir); err != nil {
		t.Fatalf("recheck linked current-city ownership: %v", err)
	} else if changed {
		t.Fatalf("linked non-owner retained current-city managed behavior:\n%s", linkedData)
	}
	canonicalFirst := string(canonicalData)
	linkedFirst := string(linkedData)
	if err := runtime.StageSessionWorkDir(cfg); err != nil {
		t.Fatalf("StageSessionWorkDir second pass: %v", err)
	}
	canonicalAfter, err := os.ReadFile(canonicalHooks)
	if err != nil {
		t.Fatalf("read canonical project hooks after second pass: %v", err)
	}
	if canonicalFirst != string(canonicalAfter) {
		t.Fatalf("repeated staging changed canonical owner hooks\nfirst:\n%s\nsecond:\n%s", canonicalFirst, canonicalAfter)
	}
	linkedSecond, err := os.ReadFile(linkedHooks)
	if err != nil {
		t.Fatalf("read linked session hooks after second pass: %v", err)
	}
	if linkedFirst != string(linkedSecond) {
		t.Fatalf("repeated staging changed linked non-owner hooks\nfirst:\n%s\nsecond:\n%s", linkedFirst, linkedSecond)
	}
}

func TestStageSessionWorkDirRejectsManagedGlobalCodexHooks(t *testing.T) {
	cityDir := t.TempDir()
	workDir := t.TempDir()
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	seedWorkDir := t.TempDir()
	installCodex(t, cityDir, seedWorkDir)
	data, err := os.ReadFile(filepath.Join(seedWorkDir, ".codex", "hooks.json"))
	if err != nil {
		t.Fatalf("read managed global fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(codexHome, "hooks.json"), data, 0o644); err != nil {
		t.Fatalf("write global hooks: %v", err)
	}

	cfg := runtime.Config{WorkDir: workDir, ProviderName: "codex"}
	configureManagedHookConvergence(&cfg, cityDir)
	err = runtime.StageSessionWorkDir(cfg)
	if err == nil {
		t.Fatal("StageSessionWorkDir succeeded with active global managed hooks")
	}
	for _, want := range []string{"global", filepath.Join(codexHome, "hooks.json"), "remove the redundant managed handlers"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q", err, want)
		}
	}
}

func TestConfigureManagedHookConvergenceArmsCodexProviderWithoutInstallList(t *testing.T) {
	cfg := runtime.Config{ProviderName: "codex"}
	configureManagedHookConvergence(&cfg, t.TempDir())
	if cfg.ConvergeManagedHooks == nil {
		t.Fatal("Codex provider omitted managed-hook convergence without install_agent_hooks")
	}
}

// TestBuildDesiredStateRuntimeStagingConvergesManagedCodexHooks verifies the
// reconciler create path carries post-staging convergence into its runtime
// configuration. It reproduces the persistent-workdir hybrid left by desired
// state materialization, then stages the returned runtime Config as the
// provider does for a live session.
func TestBuildDesiredStateRuntimeStagingConvergesManagedCodexHooks(t *testing.T) {
	overlaySrc := seedCodexOverlay(t)
	cityDir := t.TempDir()
	workDir := filepath.Join(cityDir, "persistent-worker")
	seedFuriosaHybrid(t, cityDir, workDir)

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city", Provider: "test"},
		Providers: map[string]config.ProviderSpec{
			"test": {Command: "echo", PromptMode: "none"},
		},
		PackOverlayDirs: []string{overlaySrc},
		Agents: []config.Agent{{
			Name:              "persistent-worker",
			WorkDir:           workDir,
			StartCommand:      "true",
			MaxActiveSessions: intPtr(1),
			ScaleCheck:        "echo 1",
			InstallAgentHooks: []string{"codex"},
		}},
	}

	desired := buildDesiredState("test-city", cityDir, time.Now().UTC(), cfg, runtime.NewFake(), nil, io.Discard)
	if len(desired.State) != 1 {
		t.Fatalf("desired state size = %d, want 1", len(desired.State))
	}
	var params TemplateParams
	for _, tp := range desired.State {
		params = tp
	}
	runtimeCfg := templateParamsToConfig(params)
	if runtimeCfg.ConvergeManagedHooks == nil {
		t.Fatal("reconciler runtime config omitted managed-hook convergence")
	}
	if err := runtime.StageSessionWorkDir(runtimeCfg); err != nil {
		t.Fatalf("StageSessionWorkDir: %v", err)
	}

	hooksPath := filepath.Join(workDir, ".codex", "hooks.json")
	matchers := codexSessionStartMatchers(t, hooksPath)
	if len(matchers) != 1 || matchers[0] != "startup" {
		data, _ := os.ReadFile(hooksPath)
		t.Fatalf("SessionStart matchers = %v, want exactly [startup] after reconciler runtime staging\n%s", matchers, data)
	}
	data, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatalf("read staged hooks: %v", err)
	}
	for _, command := range []string{"echo user-authored-hook", "handoff", "mail check", "nudge drain"} {
		if !strings.Contains(string(data), command) {
			t.Fatalf("reconciler runtime staging dropped %q from hooks.json:\n%s", command, data)
		}
	}
	first := string(data)
	if err := runtime.StageSessionWorkDir(runtimeCfg); err != nil {
		t.Fatalf("StageSessionWorkDir second pass: %v", err)
	}
	second, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatalf("read second staged hooks: %v", err)
	}
	if first != string(second) {
		t.Fatalf("repeated reconciler runtime staging changed hooks.json\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

// TestResolvedWorkerRuntimeConvergesManagedCodexHooks verifies the CLI resume
// path carries convergence through applyWorkerOverlayHints. It is deliberately
// separate from the reconciler test above because resume builds its runtime
// Config directly instead of routing through templateParamsToConfig.
func TestResolvedWorkerRuntimeConvergesManagedCodexHooks(t *testing.T) {
	overlaySrc := seedCodexOverlay(t)
	cityDir := t.TempDir()
	workDir := filepath.Join(cityDir, "persistent-worker")
	seedFuriosaHybrid(t, cityDir, workDir)
	installCodex(t, cityDir, workDir)

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city", Provider: "test"},
		Providers: map[string]config.ProviderSpec{
			"test": {Command: "echo", PromptMode: "none"},
		},
		PackOverlayDirs: []string{overlaySrc},
		Agents: []config.Agent{{
			Name:              "persistent-worker",
			Provider:          "test",
			WorkDir:           workDir,
			InstallAgentHooks: []string{"codex"},
		}},
	}
	runtimeCfg, err := resolvedWorkerRuntimeWithConfig(cityDir, cfg, session.Info{
		Template: "persistent-worker",
		Command:  "echo",
		WorkDir:  workDir,
	}, "")
	if err != nil {
		t.Fatalf("resolvedWorkerRuntimeWithConfig: %v", err)
	}
	if runtimeCfg.Hints.ConvergeManagedHooks == nil {
		t.Fatal("resume runtime config omitted managed-hook convergence")
	}
	if err := runtime.StageSessionWorkDir(runtimeCfg.Hints); err != nil {
		t.Fatalf("StageSessionWorkDir: %v", err)
	}

	hooksPath := filepath.Join(workDir, ".codex", "hooks.json")
	matchers := codexSessionStartMatchers(t, hooksPath)
	if len(matchers) != 1 || matchers[0] != "startup" {
		data, _ := os.ReadFile(hooksPath)
		t.Fatalf("SessionStart matchers = %v, want exactly [startup] after resume runtime staging\n%s", matchers, data)
	}
	data, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatalf("read staged hooks: %v", err)
	}
	for _, command := range []string{"echo user-authored-hook", "handoff", "mail check", "nudge drain"} {
		if !strings.Contains(string(data), command) {
			t.Fatalf("resume runtime staging dropped %q from hooks.json:\n%s", command, data)
		}
	}
}

// TestMaterializeProviderOverlays_SkipsMergeableCodexHook guards the production
// caller wiring (gcw-mnck): materializeProviderOverlaysBeforeFingerprint (the
// staging-only half of prepareTemplateResolution) must skip the reconciler-owned
// mergeable .codex/hooks.json while still staging non-mergeable overlay
// siblings. This is the observation point where skip vs non-skip staging
// diverge — the trailing hooks.Install converges either way, so only the
// staging-only state distinguishes a reverted caller wiring.
func TestMaterializeProviderOverlays_SkipsMergeableCodexHook(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "myrig")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(rig): %v", err)
	}
	overlayDir := filepath.Join(cityDir, "packs", "myrig", "overlay")
	codexOverlay := filepath.Join(overlayDir, "per-provider", "codex", ".codex")
	if err := os.MkdirAll(codexOverlay, 0o755); err != nil {
		t.Fatalf("MkdirAll(overlay): %v", err)
	}
	if err := os.WriteFile(filepath.Join(codexOverlay, "hooks.json"), []byte(`{"hooks":{"SessionStart":[]}}`), 0o644); err != nil {
		t.Fatalf("write codex hooks overlay: %v", err)
	}
	sibling := filepath.Join(overlayDir, "per-provider", "codex", "AGENTS.codex.md")
	if err := os.WriteFile(sibling, []byte("codex"), 0o644); err != nil {
		t.Fatalf("write codex sibling overlay: %v", err)
	}

	codexBase := "builtin:codex"
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:     "polecat",
			Provider: "codex",
			Scope:    "rig",
			Dir:      "myrig",
		}},
		Providers: map[string]config.ProviderSpec{
			// Explicit command + resume_command so resolution does not depend on
			// a real codex binary on PATH; base still yields the codex family.
			"codex": {Base: &codexBase, Command: "/bin/echo", ResumeCommand: "/bin/echo resume {{.SessionKey}}"},
		},
		Rigs:           []config.Rig{{Name: "myrig", Path: rigDir}},
		RigOverlayDirs: map[string][]string{"myrig": {overlayDir}},
	}

	bp := newAgentBuildParams("test-city", cityDir, cfg, runtime.NewFake(), time.Now().UTC(), nil, io.Discard)
	cfgAgent := &cfg.Agents[0]
	resolved, err := config.ResolveProvider(cfgAgent, bp.workspace, bp.providers, bp.lookPath)
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	workDir, err := resolveConfiguredWorkDir(bp.cityPath, bp.cityName, "myrig/polecat", cfgAgent, bp.rigs)
	if err != nil {
		t.Fatalf("resolveConfiguredWorkDir: %v", err)
	}
	rigName := sessionSetupContextForAgent(bp.cityPath, bp.cityName, "myrig/polecat", cfgAgent, bp.rigs).Rig

	// Staging only — hooks.Install is a separate step in prepareTemplateResolution.
	materializeProviderOverlaysBeforeFingerprint(bp, cfgAgent, resolved, "myrig/polecat", rigName, workDir, io.Discard)

	if _, err := os.Stat(filepath.Join(workDir, ".codex", "hooks.json")); !os.IsNotExist(err) {
		t.Fatalf("build_desired_state staging wrote reconciler-owned .codex/hooks.json (err=%v); caller must use the skip variant so hooks.Install is sole writer", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, "AGENTS.codex.md")); err != nil {
		t.Fatalf("non-mergeable codex overlay sibling not staged: %v", err)
	}
}

func TestPrepareTemplateResolution_T3CodexGeneratedHooksLeavesHookFileByteIdentical(t *testing.T) {
	cityDir := t.TempDir()
	workDir := filepath.Join(cityDir, "crew")
	hooksDir := filepath.Join(workDir, ".codex")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(.codex): %v", err)
	}
	original := []byte(strings.ReplaceAll(furiosaHybridCodexHooks, "__CITY__", cityDir))
	hooksPath := filepath.Join(hooksDir, "hooks.json")
	if err := os.WriteFile(hooksPath, original, 0o644); err != nil {
		t.Fatalf("write existing hooks: %v", err)
	}

	overlaySrc := seedCodexOverlay(t)
	codexBase := "builtin:codex"
	cfg := &config.City{
		Workspace: config.Workspace{
			Name:              "test-city",
			InstallAgentHooks: []string{"codex"},
		},
		Session: config.SessionConfig{Provider: "t3bridge"},
		Providers: map[string]config.ProviderSpec{
			"codex": {
				Base:          &codexBase,
				Command:       "/bin/echo",
				ResumeCommand: "/bin/echo resume {{.SessionKey}}",
			},
		},
		PackOverlayDirs: []string{overlaySrc},
		Agents: []config.Agent{{
			Name:     "crew",
			Provider: "codex",
			WorkDir:  workDir,
		}},
	}
	bp := newAgentBuildParams("test-city", cityDir, cfg, runtime.NewFake(), time.Now().UTC(), nil, io.Discard)
	resolved, err := config.ResolveProvider(&cfg.Agents[0], bp.workspace, bp.providers, bp.lookPath)
	if err != nil {
		t.Fatalf("ResolveProvider precondition: %v", err)
	}
	if family := resolvedProviderLaunchFamily(resolved); family != "codex" {
		t.Fatalf("resolved provider family = %q, want codex", family)
	}
	resolvedWorkDir, err := resolveConfiguredWorkDir(bp.cityPath, bp.cityName, "crew", &cfg.Agents[0], bp.rigs)
	if err != nil {
		t.Fatalf("resolveConfiguredWorkDir precondition: %v", err)
	}
	if resolvedWorkDir != workDir {
		t.Fatalf("resolved workdir = %q, want %q", resolvedWorkDir, workDir)
	}

	prepareTemplateResolution(bp, &cfg.Agents[0], "crew", io.Discard)
	installAgentSideEffects(bp, &cfg.Agents[0], TemplateParams{
		WorkDir:                  workDir,
		TemplateName:             "crew",
		InstanceName:             "crew",
		ResolvedProvider:         resolved,
		EffectiveSessionProvider: "t3bridge",
	}, io.Discard)

	got, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatalf("read existing hooks after generated-mode preparation: %v", err)
	}
	if string(got) != string(original) {
		t.Fatalf("generated T3 Codex preparation mutated .codex/hooks.json\nbefore:\n%s\nafter:\n%s", original, got)
	}
}
