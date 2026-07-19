package convergence

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/citylayout"
)

// writeExecutableBd creates an executable bd stub at dir/bd.
func writeExecutableBd(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %q: %v", dir, err)
	}
	path := filepath.Join(dir, "bd")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
	return path
}

// A gate condition script inherits PATH fronted with the gc-as-bd shim bin dir;
// without GC_BD_REAL the shim refuses and the check fails. Environ() must
// resolve the real bd (excluding the shim dir) so a shimmed `bd` passes through.
func TestConditionEnvInjectsGCBdReal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec-bit semantics differ on windows")
	}
	city := t.TempDir()
	writeExecutableBd(t, citylayout.ShimbinDir(city)) // the shim's bd, fronted first
	realDir := t.TempDir()
	realBd := writeExecutableBd(t, realDir)
	t.Setenv("PATH", citylayout.ShimbinDir(city)+string(os.PathListSeparator)+realDir)

	got := ""
	for _, e := range (ConditionEnv{BeadID: "gc2-1", CityPath: city}).Environ() {
		if v, ok := strings.CutPrefix(e, citylayout.RealBdEnvVar+"="); ok {
			got = v
		}
	}
	if got != realBd {
		t.Fatalf("%s = %q, want the real bd %q", citylayout.RealBdEnvVar, got, realBd)
	}
}

// When no real bd exists outside the shim dir, GC_BD_REAL is omitted (rather
// than pointing at the shim, which would recurse).
func TestConditionEnvOmitsGCBdRealWhenNoRealBd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec-bit semantics differ on windows")
	}
	city := t.TempDir()
	writeExecutableBd(t, citylayout.ShimbinDir(city)) // only the shim's bd
	t.Setenv("PATH", citylayout.ShimbinDir(city))

	for _, e := range (ConditionEnv{CityPath: city}).Environ() {
		if strings.HasPrefix(e, citylayout.RealBdEnvVar+"=") {
			t.Fatalf("expected no %s when only the shim bd is on PATH; got %q", citylayout.RealBdEnvVar, e)
		}
	}
}

// condEnvPATH extracts the PATH entry from an Environ() slice.
func condEnvPATH(env []string) string {
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "PATH="); ok {
			return v
		}
	}
	return ""
}

// TestConditionEnvFrontsShimWhenInstalled pins that a condition script's PATH is
// fronted with the shim bin dir when the shim is installed (gc symlink present,
// so its `bd` routes through the warm controller), and is NOT fronted when the
// shim is absent (bd_shim=off / not installed) — no config threading needed.
func TestConditionEnvFrontsShimWhenInstalled(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	city := t.TempDir()
	shim := citylayout.ShimbinDir(city)
	sep := string(os.PathListSeparator)

	// Not installed (no gc symlink) -> PATH is not fronted with the shim dir.
	if got := condEnvPATH((ConditionEnv{CityPath: city}).Environ()); strings.HasPrefix(got, shim+sep) {
		t.Fatalf("PATH fronted with shim dir before install: %q", got)
	}

	// Install the shim gc symlink -> ShimInstalled true -> PATH fronted.
	if err := os.MkdirAll(shim, 0o755); err != nil {
		t.Fatalf("mkdir shim: %v", err)
	}
	if err := os.Symlink(filepath.Join(city, "gc-real"), citylayout.ShimbinGCPath(city)); err != nil {
		t.Fatalf("symlink shim gc: %v", err)
	}
	got := condEnvPATH((ConditionEnv{CityPath: city}).Environ())
	if !strings.HasPrefix(got, shim+sep) && got != shim {
		t.Fatalf("PATH not fronted with shim dir after install: %q", got)
	}
}
