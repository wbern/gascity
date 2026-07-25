package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestStageDirPreservesBestEffortOverlayWarnings(t *testing.T) {
	t.Parallel()

	srcDir := t.TempDir()
	dstDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(srcDir, "ok.txt"), []byte("copied"), 0o644); err != nil {
		t.Fatalf("write ok overlay file: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(srcDir, "blocked"), 0o755); err != nil {
		t.Fatalf("mkdir blocked src dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "blocked", "nested.txt"), []byte("ignored"), 0o644); err != nil {
		t.Fatalf("write blocked overlay file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dstDir, "blocked"), []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("write blocked dst file: %v", err)
	}

	if err := StageDir(srcDir, dstDir); err != nil {
		t.Fatalf("StageDir() error = %v, want nil", err)
	}

	data, err := os.ReadFile(filepath.Join(dstDir, "ok.txt"))
	if err != nil {
		t.Fatalf("read copied overlay file: %v", err)
	}
	if string(data) != "copied" {
		t.Fatalf("copied overlay file = %q, want %q", string(data), "copied")
	}
}

func TestStageWorkDirSkipsCopyWhenSourceAlreadyMatchesResolvedDestination(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	src := filepath.Join(workDir, "seed.txt")
	if err := os.WriteFile(src, []byte("seed"), 0o644); err != nil {
		t.Fatalf("write source file: %v", err)
	}

	if err := StageWorkDir(workDir, "", []CopyEntry{{Src: src}}); err != nil {
		t.Fatalf("StageWorkDir() error = %v, want nil", err)
	}

	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read staged source file: %v", err)
	}
	if string(data) != "seed" {
		t.Fatalf("staged source file = %q, want %q", string(data), "seed")
	}
}

func TestStageWorkDirFailsWhenOverlayCopyWarns(t *testing.T) {
	t.Parallel()

	srcDir := t.TempDir()
	workDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(srcDir, "ok.txt"), []byte("copied"), 0o644); err != nil {
		t.Fatalf("write ok overlay file: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(srcDir, "blocked"), 0o755); err != nil {
		t.Fatalf("mkdir blocked src dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "blocked", "nested.txt"), []byte("ignored"), 0o644); err != nil {
		t.Fatalf("write blocked overlay file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "blocked"), []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("write blocked dst file: %v", err)
	}

	err := StageWorkDir(workDir, srcDir, nil)
	if err == nil {
		t.Fatal("StageWorkDir() succeeded, want overlay staging error")
	}
	if data, readErr := os.ReadFile(filepath.Join(workDir, "ok.txt")); readErr != nil {
		t.Fatalf("read copied overlay file: %v", readErr)
	} else if string(data) != "copied" {
		t.Fatalf("copied overlay file = %q, want %q", string(data), "copied")
	}
}

func TestStageSessionWorkDirUsesConcreteProviderOverlayName(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	packOverlay := t.TempDir()

	kiroConfig := filepath.Join(packOverlay, "per-provider", "kiro", ".kiro", "agents", "gascity.json")
	if err := os.MkdirAll(filepath.Dir(kiroConfig), 0o755); err != nil {
		t.Fatalf("mkdir Kiro overlay: %v", err)
	}
	if err := os.WriteFile(kiroConfig, []byte(`{"name":"gascity"}`), 0o644); err != nil {
		t.Fatalf("write Kiro overlay: %v", err)
	}
	claudeConfig := filepath.Join(packOverlay, "per-provider", "claude", "CLAUDE.md")
	if err := os.MkdirAll(filepath.Dir(claudeConfig), 0o755); err != nil {
		t.Fatalf("mkdir Claude overlay: %v", err)
	}
	if err := os.WriteFile(claudeConfig, []byte("claude instructions"), 0o644); err != nil {
		t.Fatalf("write Claude overlay: %v", err)
	}

	err := StageSessionWorkDir(Config{
		WorkDir:             workDir,
		ProviderName:        "claude",
		ProviderOverlayName: "kiro",
		PackOverlayDirs:     []string{packOverlay},
	})
	if err != nil {
		t.Fatalf("StageSessionWorkDir: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(workDir, ".kiro", "agents", "gascity.json")); err != nil {
		t.Fatalf("read staged Kiro config: %v", err)
	} else if string(got) != `{"name":"gascity"}` {
		t.Fatalf("staged Kiro config = %q, want gascity config", got)
	}
	if _, err := os.Stat(filepath.Join(workDir, "CLAUDE.md")); err == nil {
		t.Fatal("staged Claude overlay for Kiro provider inheriting Claude launch behavior")
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat Claude overlay: %v", err)
	}
}

func TestStageProviderOverlayDirSkippingPreparedPathsPreservesManagedHooks(t *testing.T) {
	t.Parallel()

	overlayDir := t.TempDir()
	workDir := t.TempDir()
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
	otherManagedFile := filepath.Join(overlayDir, ".gemini", "settings.json")
	if err := os.MkdirAll(filepath.Dir(otherManagedFile), 0o755); err != nil {
		t.Fatalf("mkdir other managed file directory: %v", err)
	}
	if err := os.WriteFile(otherManagedFile, []byte(`{"hooks":{}}`), 0o644); err != nil {
		t.Fatalf("write other managed file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(overlayDir, "AGENTS.md"), []byte("overlay instructions"), 0o644); err != nil {
		t.Fatalf("write overlay instructions: %v", err)
	}

	if err := StageProviderOverlayDirSkippingPaths(overlayDir, workDir, []string{"codex"}, []string{filepath.Join(".codex", "hooks.json")}, nil); err != nil {
		t.Fatalf("StageProviderOverlayDirSkippingPaths: %v", err)
	}
	if got, err := os.ReadFile(managedHook); err != nil {
		t.Fatalf("read managed hook: %v", err)
	} else if string(got) != canonicalHook {
		t.Fatalf("managed hook = %q, want canonical controller-owned bytes", got)
	}
	if got, err := os.ReadFile(filepath.Join(workDir, "AGENTS.md")); err != nil {
		t.Fatalf("read staged instruction file: %v", err)
	} else if string(got) != "overlay instructions" {
		t.Fatalf("staged instructions = %q, want overlay content", got)
	}
	if _, err := os.Stat(filepath.Join(workDir, ".gemini", "settings.json")); err != nil {
		t.Fatalf("unprepared mergeable file was not staged: %v", err)
	}
}

func TestStageSessionWorkDirStagesMergeableHooksWithoutControllerPreparation(t *testing.T) {
	t.Parallel()

	overlayDir := t.TempDir()
	workDir := t.TempDir()
	overlayHook := filepath.Join(overlayDir, ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(overlayHook), 0o755); err != nil {
		t.Fatalf("mkdir overlay hook directory: %v", err)
	}
	const stagedHook = `{"hooks":{"SessionStart":[]}}`
	if err := os.WriteFile(overlayHook, []byte(stagedHook), 0o644); err != nil {
		t.Fatalf("write overlay hook: %v", err)
	}

	if err := StageSessionWorkDir(Config{WorkDir: workDir, ProviderName: "codex", PackOverlayDirs: []string{overlayDir}}); err != nil {
		t.Fatalf("StageSessionWorkDir: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(workDir, ".codex", "hooks.json")); err != nil {
		t.Fatalf("read staged hook: %v", err)
	} else {
		var want, actual map[string]any
		if err := json.Unmarshal([]byte(stagedHook), &want); err != nil {
			t.Fatalf("unmarshal expected staged hook: %v", err)
		}
		if err := json.Unmarshal(got, &actual); err != nil {
			t.Fatalf("unmarshal staged hook: %v", err)
		}
		if !reflect.DeepEqual(actual, want) {
			t.Fatalf("staged hook = %#v, want %#v", actual, want)
		}
	}
}

// TestStageSessionWorkDirFallsBackToFamilyOverlayWhenConcreteAbsent guards
// gc-6bw8o: a custom provider with base="builtin:pi" resolves
// ProviderOverlayName="pi-vllm", which has no per-provider/pi-vllm/ overlay dir.
// The family overlay (per-provider/pi/, where gc-hooks.js lives) must still
// stage, otherwise the harness never signals ready and the agent churns. Unlike
// Kiro, the concrete overlay is absent, so the launch family is used.
func TestStageSessionWorkDirFallsBackToFamilyOverlayWhenConcreteAbsent(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	packOverlay := t.TempDir()

	hook := filepath.Join(packOverlay, "per-provider", "pi", ".pi", "extensions", "gc-hooks.js")
	if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
		t.Fatalf("mkdir pi overlay: %v", err)
	}
	if err := os.WriteFile(hook, []byte("// gc hook"), 0o644); err != nil {
		t.Fatalf("write pi hook: %v", err)
	}

	err := StageSessionWorkDir(Config{
		WorkDir:             workDir,
		ProviderName:        "pi",
		ProviderOverlayName: "pi-vllm",
		PackOverlayDirs:     []string{packOverlay},
	})
	if err != nil {
		t.Fatalf("StageSessionWorkDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, ".pi", "extensions", "gc-hooks.js")); err != nil {
		t.Fatalf("family pi overlay not staged for custom pi-vllm provider (gc-6bw8o): %v", err)
	}
}

func TestStageSessionWorkDirWithWarningsSurfacesKiroPreservationWarning(t *testing.T) {
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
	err := StageSessionWorkDirWithWarnings(Config{
		WorkDir:         workDir,
		ProviderName:    "kiro",
		PackOverlayDirs: []string{packOverlay},
	}, &warnings)
	if err != nil {
		t.Fatalf("StageSessionWorkDirWithWarnings: %v", err)
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

func TestStageProviderOverlayDirIgnoresWarningWriterFailure(t *testing.T) {
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

	err := StageProviderOverlayDir(packOverlay, workDir, []string{"kiro"}, failingWriter{})
	if err != nil {
		t.Fatalf("StageProviderOverlayDir: %v", err)
	}
	data, err := os.ReadFile(projectInstructions)
	if err != nil {
		t.Fatalf("read AGENTS.md: %v", err)
	}
	if string(data) != "project instructions" {
		t.Fatalf("AGENTS.md = %q, want project instructions preserved", string(data))
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("writer unavailable")
}
