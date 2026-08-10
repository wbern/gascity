package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// codexHooksOverlaySrc seeds a provider overlay dir with a codex hooks file
// (mergeable) and a non-mergeable sibling under per-provider/codex/, returning
// the overlay source root.
func codexHooksOverlaySrc(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	codexDir := filepath.Join(src, "per-provider", "codex", ".codex")
	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		t.Fatalf("mkdir codex overlay: %v", err)
	}
	if err := os.WriteFile(filepath.Join(codexDir, "hooks.json"), []byte(`{"hooks":{"SessionStart":[]}}`), 0o644); err != nil {
		t.Fatalf("write codex hooks overlay: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "per-provider", "codex", "AGENTS.codex.md"), []byte("codex"), 0o644); err != nil {
		t.Fatalf("write codex sibling overlay: %v", err)
	}
	return src
}

// TestStageProviderOverlayDirSkippingMergeableSkipsCodexHooks is the horizon
// guard (gcw-mnck invariant #3): the build_desired_state staging entry point
// must skip reconciler-owned mergeable files while still staging non-mergeable
// siblings, so hooks.Install remains the sole writer in the home dir.
func TestStageProviderOverlayDirSkippingMergeableSkipsCodexHooks(t *testing.T) {
	t.Parallel()

	src := codexHooksOverlaySrc(t)
	workDir := t.TempDir()

	if err := StageProviderOverlayDirSkippingMergeable(src, workDir, []string{"codex"}, nil); err != nil {
		t.Fatalf("StageProviderOverlayDirSkippingMergeable: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, ".codex", "hooks.json")); !os.IsNotExist(err) {
		t.Fatalf(".codex/hooks.json staged by home-dir path (err=%v); must be skipped so hooks.Install is sole writer", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, "AGENTS.codex.md")); err != nil {
		t.Fatalf("non-mergeable sibling should still stage: %v", err)
	}
}

// TestStageProviderOverlayDirStagesCodexHooks locks the no-regression contract
// (gcw-mnck invariant #3 / #2): the runtime task-worktree path (plain
// StageProviderOverlayDir, used by StageSessionWorkDir) still writes the codex
// hook file, which is the only hook source for live task sessions.
func TestStageProviderOverlayDirStagesCodexHooks(t *testing.T) {
	t.Parallel()

	src := codexHooksOverlaySrc(t)
	workDir := t.TempDir()

	if err := StageProviderOverlayDir(src, workDir, []string{"codex"}, nil); err != nil {
		t.Fatalf("StageProviderOverlayDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, ".codex", "hooks.json")); err != nil {
		t.Fatalf("runtime path must stage .codex/hooks.json (codex live-session hook source): %v", err)
	}
}

// TestStageSessionWorkDirStagesFunctionalCodexHooks is the no-regression test
// (gcw-mnck #2) at the session-staging boundary: StageSessionWorkDir, invoked
// on every codex task-session Start, must still write a functional
// .codex/hooks.json (SessionStart present). The fix must not touch this path.
func TestStageSessionWorkDirStagesFunctionalCodexHooks(t *testing.T) {
	t.Parallel()

	src := codexHooksOverlaySrc(t)
	workDir := t.TempDir()

	if err := StageSessionWorkDir(Config{
		WorkDir:         workDir,
		ProviderName:    "codex",
		PackOverlayDirs: []string{src},
	}); err != nil {
		t.Fatalf("StageSessionWorkDir: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(workDir, ".codex", "hooks.json"))
	if err != nil {
		t.Fatalf("codex task worktree missing .codex/hooks.json (P1 regression): %v", err)
	}
	if !strings.Contains(string(data), "SessionStart") {
		t.Fatalf("staged codex hooks not functional, want SessionStart: %s", data)
	}
}

func TestStageSessionWorkDirGeneratedCodexFlagsLeavesHookFileByteIdentical(t *testing.T) {
	t.Parallel()

	src := codexHooksOverlaySrc(t)
	workDir := t.TempDir()
	hooksDir := filepath.Join(workDir, ".codex")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatalf("mkdir .codex: %v", err)
	}
	hooksPath := filepath.Join(hooksDir, "hooks.json")
	original := []byte(`{"hooks":{"Stop":[{"matcher":"","hooks":[{"type":"command","command":"echo custom"}]}]}}`)
	if err := os.WriteFile(hooksPath, original, 0o644); err != nil {
		t.Fatalf("write existing hooks: %v", err)
	}

	if err := StageSessionWorkDir(Config{
		WorkDir:         workDir,
		ProviderName:    "codex",
		PackOverlayDirs: []string{src},
		CodexSessionFlags: func() *CodexSessionFlagsPayload {
			payload := NewCodexSessionFlagsPayload(CodexSessionConfig{FeaturesHooks: true, BypassHookTrust: true})
			return &payload
		}(),
	}); err != nil {
		t.Fatalf("StageSessionWorkDir: %v", err)
	}

	got, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatalf("read existing hooks: %v", err)
	}
	if string(got) != string(original) {
		t.Fatalf("generated-mode runtime staging changed .codex/hooks.json\nbefore: %s\nafter:  %s", original, got)
	}
	if _, err := os.Stat(filepath.Join(workDir, "AGENTS.codex.md")); err != nil {
		t.Fatalf("generated-mode runtime staging dropped non-hook Codex sibling: %v", err)
	}
}
