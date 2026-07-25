package tmux

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestStageStartFilesSurfacesKiroPreservationWarning(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	packOverlay := t.TempDir()

	fallbackInstructions := filepath.Join(packOverlay, "per-provider", "kiro", "AGENTS.md")
	if err := os.MkdirAll(filepath.Dir(fallbackInstructions), 0o755); err != nil {
		t.Fatalf("mkdir Kiro overlay: %v", err)
	}
	if err := os.WriteFile(fallbackInstructions, []byte("fallback instructions"), 0o644); err != nil {
		t.Fatalf("write Kiro fallback instructions: %v", err)
	}
	projectInstructions := filepath.Join(workDir, "AGENTS.md")
	if err := os.WriteFile(projectInstructions, []byte("project instructions"), 0o600); err != nil {
		t.Fatalf("write project instructions: %v", err)
	}

	var warnings bytes.Buffer
	err := stageStartFiles(runtime.Config{
		WorkDir:         workDir,
		ProviderName:    "kiro",
		PackOverlayDirs: []string{packOverlay},
	}, &warnings)
	if err != nil {
		t.Fatalf("stageStartFiles: %v", err)
	}
	if got := warnings.String(); !strings.Contains(got, "overlay: preserving existing") || !strings.Contains(got, "AGENTS.md") {
		t.Fatalf("warnings = %q, want Kiro preservation warning", got)
	}
	data, err := os.ReadFile(projectInstructions)
	if err != nil {
		t.Fatalf("read AGENTS.md: %v", err)
	}
	if string(data) != "project instructions" {
		t.Fatalf("AGENTS.md = %q, want project instructions preserved", string(data))
	}
}

func TestStageStartFilesPreservesControllerPreparedMergeableHooks(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	overlayDir := t.TempDir()
	managedHook := filepath.Join(workDir, ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(managedHook), 0o755); err != nil {
		t.Fatalf("mkdir managed hook directory: %v", err)
	}
	const canonicalHook = `{"hooks":{"SessionStart":[{"matcher":"startup","hooks":[{"type":"command","command":"gc --city /city prime --hook --hook-format codex"}]}]}}`
	if err := os.WriteFile(managedHook, []byte(canonicalHook), 0o644); err != nil {
		t.Fatalf("write managed hook: %v", err)
	}
	overlayHook := filepath.Join(overlayDir, ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(overlayHook), 0o755); err != nil {
		t.Fatalf("mkdir overlay hook directory: %v", err)
	}
	if err := os.WriteFile(overlayHook, []byte(`{"hooks":{"SessionStart":[]}}`), 0o644); err != nil {
		t.Fatalf("write overlay hook: %v", err)
	}
	if err := os.WriteFile(filepath.Join(overlayDir, "AGENTS.md"), []byte("overlay instructions"), 0o644); err != nil {
		t.Fatalf("write overlay instructions: %v", err)
	}

	if err := stageStartFiles(runtime.Config{
		WorkDir:                workDir,
		ProviderName:           "codex",
		PackOverlayDirs:        []string{overlayDir},
		PreparedMergeablePaths: []string{filepath.Join(".codex", "hooks.json")},
	}, nil); err != nil {
		t.Fatalf("stageStartFiles: %v", err)
	}
	if got, err := os.ReadFile(managedHook); err != nil {
		t.Fatalf("read managed hook: %v", err)
	} else if string(got) != canonicalHook {
		t.Fatalf("managed hook = %q, want canonical controller-owned bytes", got)
	}
	if got, err := os.ReadFile(filepath.Join(workDir, "AGENTS.md")); err != nil {
		t.Fatalf("read staged instructions: %v", err)
	} else if string(got) != "overlay instructions" {
		t.Fatalf("staged instructions = %q, want overlay content", got)
	}
}

func TestStageStartFilesKeepsScaffoldOutOfSpawnerCWD(t *testing.T) {
	root := t.TempDir()
	sharedWorktree := filepath.Join(root, "shared-builder")
	beadSlug := "ga-ajw1no-1-as-a-maintainer-i-can-reproduce-stray-session-scaffold-leakage"
	leakedWorkDir := filepath.Join(sharedWorktree, beadSlug)
	workDir := filepath.Join(root, "city", ".gc", "worktrees", "gascity", "builder", beadSlug)
	packOverlay := filepath.Join(root, "city", "packs", "core", "overlay")

	writeTmuxScaffoldFixture(t, filepath.Join(packOverlay, ".claude", "skills", "triage", "SKILL.md"), "---\nname: triage\n---\n")
	writeTmuxScaffoldFixture(t, filepath.Join(packOverlay, ".codex", "hooks.json"), `{"hooks":{"SessionStart":[]}}`+"\n")
	writeTmuxScaffoldFixture(t, filepath.Join(packOverlay, ".gc", "settings.json"), "{}\n")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", workDir, err)
	}
	if err := os.MkdirAll(sharedWorktree, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", sharedWorktree, err)
	}
	t.Chdir(sharedWorktree)

	var warnings bytes.Buffer
	err := stageStartFiles(runtime.Config{
		WorkDir:             workDir,
		ProviderName:        "codex",
		ProviderOverlayName: "codex",
		PackOverlayDirs:     []string{packOverlay},
	}, &warnings)
	if err != nil {
		t.Fatalf("stageStartFiles: %v", err)
	}

	for _, rel := range []string{
		filepath.Join(".claude", "skills", "triage", "SKILL.md"),
		filepath.Join(".codex", "hooks.json"),
	} {
		if _, err := os.Stat(filepath.Join(workDir, rel)); err != nil {
			t.Errorf("target scaffold %s missing under workdir %q: %v", rel, workDir, err)
		}
	}
	// A top-level .gc/ in the overlay source is a runtime mirror and must never
	// be staged into a session workdir (overlay.skipRuntimeMirror). The session's
	// own .gc/settings.json is staged separately through the hook-file path, not
	// copied verbatim from the pack overlay.
	if _, err := os.Stat(filepath.Join(workDir, ".gc", "settings.json")); !os.IsNotExist(err) {
		t.Errorf("overlay .gc runtime mirror must not be staged under workdir %q (stat err = %v)", workDir, err)
	}
	if _, err := os.Stat(leakedWorkDir); err == nil {
		t.Fatalf("shared cwd contains stray bead-slug scaffold directory %q; scaffold must stay under %q", leakedWorkDir, workDir)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat leaked workdir %q: %v", leakedWorkDir, err)
	}
}

func writeTmuxScaffoldFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}
