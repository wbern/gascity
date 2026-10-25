package runtime

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
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

func TestStagePathPreservesSameFile(t *testing.T) {
	for _, viaDirectory := range []bool{false, true} {
		work := t.TempDir()
		src := filepath.Join(work, "hooks.json")
		if err := os.WriteFile(src, []byte("preserve-hook"), 0o600); err != nil {
			t.Fatal(err)
		}
		dst := src
		if viaDirectory {
			dst = work
		}
		if err := StagePath(src, dst); err != nil {
			t.Fatal(err)
		}
		if got, err := os.ReadFile(src); err != nil || string(got) != "preserve-hook" {
			t.Fatalf("self-stage truncated hook: %q %v", got, err)
		}
	}
}

func TestStagePathPreservesAtomicReplacementAfterProbe(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "hooks.json")
	if err := os.WriteFile(src, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, sourceInfo, err := effectiveStageDestination(src, src)
	if err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(dir, "replacement")
	if err := os.WriteFile(replacement, []byte("new hook"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, src); err != nil {
		t.Fatal(err)
	}
	current, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(sourceInfo, current) {
		t.Fatal("fixture did not replace the inode")
	}
	if err := stagePathAfterProbe(src, src, src, sourceInfo); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(src); err != nil || string(got) != "new hook" {
		t.Fatalf("atomic replacement self-copy lost hook: %q %v", got, err)
	}
}

func TestStagePathRespectsSymlinkDotDot(t *testing.T) {
	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	if err := os.MkdirAll(filepath.Join(realDir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(realDir, "sub"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realDir, "target"), []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "target")
	if err := os.WriteFile(dst, []byte("destination"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Join/Clean would erase this meaningful traversal through a symlink.
	src := dir + "/link/../target"
	if err := StagePath(src, dst); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(dst); err != nil || string(got) != "source" {
		t.Fatalf("non-equivalent OS paths treated as same: %q %v", got, err)
	}
}

func TestStagePathPreservesHardlinkAlias(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source")
	dst := filepath.Join(dir, "alias")
	if err := os.WriteFile(src, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(src, dst); err != nil {
		t.Fatal(err)
	}
	if err := StagePath(src, dst); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(src); err != nil || string(got) != "preserve" {
		t.Fatalf("hardlink copy clobbered source: %q %v", got, err)
	}
}

func TestStagePathMissingSourcePreservesNoOp(t *testing.T) {
	src := filepath.Join(t.TempDir(), "missing")
	for _, dst := range []string{src, src + "-other"} {
		if err := StagePath(src, dst); err != nil {
			t.Fatalf("missing source changed best-effort contract: %v", err)
		}
		if _, err := os.Stat(dst); !os.IsNotExist(err) {
			t.Fatalf("missing-source stage created destination: %v", err)
		}
	}
}
