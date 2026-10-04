package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
)

// writeControllerShimCity builds a city whose bd-shim is installed
// (<city>/.gc/shimbin/{gc,bd}, bd resolving to a binary named "bdshim") and a
// process PATH that fronts the shim bin dir ahead of the real bd — the launchd
// supervisor PATH on gc2. The fake shim records that it ran and fails the way
// bdshim's self-recursion guard does without GC_BD_REAL; the real bd records
// that it ran and succeeds.
func writeControllerShimCity(t *testing.T) (cityDir, realBd, shimMarker, realMarker string) {
	t.Helper()
	cityDir = t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	markers := t.TempDir()
	shimMarker = filepath.Join(markers, "shim-ran")
	realMarker = filepath.Join(markers, "real-bd-ran")

	shimTarget := filepath.Join(t.TempDir(), "bdshim")
	writeExecutable(t, shimTarget, "#!/bin/sh\nprintf ran > \""+shimMarker+"\"\necho 'bdshim: refusing recursive bdshim passthrough' >&2\nexit 1\n")
	gcTarget := filepath.Join(t.TempDir(), "gc")
	writeExecutable(t, gcTarget, "#!/bin/sh\nexit 0\n")
	shimbin := citylayout.ShimbinDir(cityDir)
	if err := os.MkdirAll(shimbin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(shimTarget, filepath.Join(shimbin, "bd")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(gcTarget, citylayout.ShimbinGCPath(cityDir)); err != nil {
		t.Fatal(err)
	}

	realDir := t.TempDir()
	realBd = filepath.Join(realDir, "bd")
	writeExecutable(t, realBd, "#!/bin/sh\nprintf ran > \""+realMarker+"\"\n")

	t.Setenv("PATH", shimbin+string(os.PathListSeparator)+realDir)
	t.Setenv("BD_BIN", "")
	t.Setenv(citylayout.RealBdEnvVar, "")
	return cityDir, realBd, shimMarker, realMarker
}

// TestControllerBdBypassesCityShimOnPath pins the gcw-o4qtt S5 regression: the
// supervisor's own bd invocations (city/rig BdStore, provider scripts) must
// exec the real bd even when the city's bd-shim bin dir is on the process PATH
// and GC_BD_REAL is unset. Routing them through the shim recursed into the
// shim's self-passthrough guard and failed every city's init with
// "bd list: ... refusing recursive bdshim passthrough".
func TestControllerBdBypassesCityShimOnPath(t *testing.T) {
	cityDir, realBd, shimMarker, realMarker := writeControllerShimCity(t)

	env := map[string]string{}
	if err := applyWorkspacePinnedBdBinary(env, cityDir); err != nil {
		t.Fatalf("applyWorkspacePinnedBdBinary() error = %v", err)
	}
	if got := env["BD_BIN"]; got != realBd {
		t.Fatalf("env[BD_BIN] = %q, want the real bd %q (never the city shim)", got, realBd)
	}

	if _, err := beads.ExecCommandRunnerWithEnv(env)(cityDir, "bd", "list"); err != nil {
		t.Fatalf("controller bd exec with composed env: %v", err)
	}
	if _, err := os.Stat(shimMarker); err == nil {
		t.Fatal("controller bd exec ran the city bd-shim; it must bypass the shim")
	}
	if _, err := os.Stat(realMarker); err != nil {
		t.Fatalf("real bd did not run: %v", err)
	}
}

// TestControllerBdBypassesCityShimViaRuntimeEnv covers the composed runtime env
// the city BdStore runner actually uses (bdRuntimeEnvWithError), not just the
// helper, so a future caller that bypasses applyWorkspacePinnedBdBinary is
// caught.
func TestControllerBdBypassesCityShimViaRuntimeEnv(t *testing.T) {
	cityDir, realBd, _, _ := writeControllerShimCity(t)
	env := cityRuntimeEnvMapForCity(cityDir)
	if err := applyWorkspacePinnedBdBinary(env, cityDir); err != nil {
		t.Fatalf("applyWorkspacePinnedBdBinary() error = %v", err)
	}
	if got := env["BD_BIN"]; got != realBd {
		t.Fatalf("runtime env BD_BIN = %q, want real bd %q", got, realBd)
	}
}

// TestControllerBdKeepsExplicitWorkspacePinOverShimFallback ensures the shim
// fallback never overrides a declared workspace.env BD_BIN.
func TestControllerBdKeepsExplicitWorkspacePinOverShimFallback(t *testing.T) {
	cityDir, _, _, _ := writeControllerShimCity(t)
	pinned := filepath.Join(t.TempDir(), "bd-pinned")
	writeExecutable(t, pinned, "#!/bin/sh\nexit 0\n")
	toml := "[workspace]\nname = \"demo\"\n[workspace.env]\nBD_BIN = \"" + pinned + "\"\n"
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	if err := applyWorkspacePinnedBdBinary(env, cityDir); err != nil {
		t.Fatalf("applyWorkspacePinnedBdBinary() error = %v", err)
	}
	if got := env["BD_BIN"]; got != pinned {
		t.Fatalf("env[BD_BIN] = %q, want declared pin %q", got, pinned)
	}
}

// TestControllerBdShimFallbackLeavesEmptyPinWhenNoRealBd keeps the prior
// contract when nothing but the shim provides bd: BD_BIN stays empty (masking
// any inherited value) rather than naming the shim.
func TestControllerBdShimFallbackLeavesEmptyPinWhenNoRealBd(t *testing.T) {
	cityDir, _, _, _ := writeControllerShimCity(t)
	t.Setenv("PATH", citylayout.ShimbinDir(cityDir))
	env := map[string]string{}
	if err := applyWorkspacePinnedBdBinary(env, cityDir); err != nil {
		t.Fatalf("applyWorkspacePinnedBdBinary() error = %v", err)
	}
	if v, ok := env["BD_BIN"]; !ok || v != "" {
		t.Fatalf("env[BD_BIN] = %q (present=%v), want present and empty", v, ok)
	}
	if strings.Contains(env["BD_BIN"], "shimbin") {
		t.Fatalf("BD_BIN must never name the city shim: %q", env["BD_BIN"])
	}
}
