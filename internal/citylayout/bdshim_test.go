package citylayout

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestShimbinDir(t *testing.T) {
	got := ShimbinDir("/cities/gc2")
	want := filepath.Join("/cities/gc2", RuntimeRoot, "shimbin")
	if got != want {
		t.Fatalf("ShimbinDir = %q, want %q", got, want)
	}
}

// writeExecutable creates an executable bd file in dir for PATH-resolution tests.
func writeExecutable(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "bd")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("writing %q: %v", path, err)
	}
	return path
}

func TestResolveRealBdExcludingDir_SkipsShimDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec-bit semantics differ on windows")
	}
	shimDir := t.TempDir()
	realDir := t.TempDir()
	// A `bd` exists in BOTH the shim dir (first on PATH) and the real dir.
	writeExecutable(t, shimDir)
	realBd := writeExecutable(t, realDir)

	// Shim dir is fronted first, exactly as a shimmed session/controller PATH.
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+realDir)

	got, err := ResolveRealBdExcludingDir(shimDir)
	if err != nil {
		t.Fatalf("ResolveRealBdExcludingDir: %v", err)
	}
	if got != realBd {
		t.Fatalf("resolved %q, want the real bd %q (must skip the shim dir)", got, realBd)
	}
}

func TestResolveRealBdExcludingDir_NoRealBd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec-bit semantics differ on windows")
	}
	shimDir := t.TempDir()
	writeExecutable(t, shimDir) // only the shim's bd exists
	t.Setenv("PATH", shimDir)

	if _, err := ResolveRealBdExcludingDir(shimDir); err == nil {
		t.Fatal("expected error when the only bd is inside the excluded shim dir")
	}
}

func TestResolveRealBd_ComposesShimbinDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec-bit semantics differ on windows")
	}
	city := t.TempDir()
	t.Setenv(RealBdEnvVar, "")
	shimDir := ShimbinDir(city)
	if err := os.MkdirAll(shimDir, 0o755); err != nil {
		t.Fatalf("mkdir shim dir: %v", err)
	}
	realDir := t.TempDir()
	writeExecutable(t, shimDir)
	realBd := writeExecutable(t, realDir)
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+realDir)

	got, err := ResolveRealBd(city)
	if err != nil {
		t.Fatalf("ResolveRealBd: %v", err)
	}
	if got != realBd {
		t.Fatalf("ResolveRealBd = %q, want %q", got, realBd)
	}
}

func TestResolveRealBd_PreservesExplicitPassthrough(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec-bit semantics differ on windows")
	}
	city := t.TempDir()
	shimDir := ShimbinDir(city)
	if err := os.MkdirAll(shimDir, 0o755); err != nil {
		t.Fatalf("mkdir shim dir: %v", err)
	}
	pathFirstDir := t.TempDir()
	writeExecutable(t, pathFirstDir)
	explicitDir := t.TempDir()
	explicit := writeExecutable(t, explicitDir)
	t.Setenv("PATH", pathFirstDir+string(os.PathListSeparator)+explicitDir)
	t.Setenv(RealBdEnvVar, explicit)

	got, err := ResolveRealBd(city)
	if err != nil {
		t.Fatalf("ResolveRealBd: %v", err)
	}
	if got != explicit {
		t.Fatalf("ResolveRealBd = %q, want explicit passthrough %q", got, explicit)
	}
}
