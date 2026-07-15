package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFakeBd writes an executable `bd` stub into dir and returns its path.
func writeFakeBd(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	path := filepath.Join(dir, "bd")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("writing fake bd: %v", err)
	}
	return path
}

func TestEnsureCityBdShimbinCreatesSymlinks(t *testing.T) {
	cityPath := t.TempDir()
	realBdDir := t.TempDir()
	writeFakeBd(t, realBdDir)
	t.Setenv("PATH", realBdDir)

	if err := ensureCityBdShimbin(cityPath, io.Discard); err != nil {
		t.Fatalf("ensureCityBdShimbin: %v", err)
	}

	gcLink := cityBdShimbinGCPath(cityPath)
	bdLink := filepath.Join(cityBdShimbinDir(cityPath), "bd")
	if !isSymlink(gcLink) {
		t.Fatalf("gc symlink %q not created", gcLink)
	}
	if !isSymlink(bdLink) {
		t.Fatalf("bd symlink %q not created", bdLink)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	if got, _ := os.Readlink(gcLink); got != exe {
		t.Fatalf("gc symlink -> %q, want %q", got, exe)
	}
	// bd -> the in-dir gc symlink (single source of truth; bd follows gc refresh).
	if got, _ := os.Readlink(bdLink); got != gcLink {
		t.Fatalf("bd symlink -> %q, want %q", got, gcLink)
	}

	// Clobber-safety: the real bd install dir is never written to.
	entries, err := os.ReadDir(realBdDir)
	if err != nil {
		t.Fatalf("reading real bd dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "bd" {
		t.Fatalf("real bd dir mutated: %v", entries)
	}
}

func TestEnsureCityBdShimbinIdempotentAndAtomic(t *testing.T) {
	cityPath := t.TempDir()
	realBdDir := t.TempDir()
	writeFakeBd(t, realBdDir)
	t.Setenv("PATH", realBdDir)

	if err := ensureCityBdShimbin(cityPath, io.Discard); err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	gcLink := cityBdShimbinGCPath(cityPath)
	before, err := os.Lstat(gcLink)
	if err != nil {
		t.Fatalf("lstat gc link: %v", err)
	}

	if err := ensureCityBdShimbin(cityPath, io.Discard); err != nil {
		t.Fatalf("second ensure: %v", err)
	}

	// A converged refresh rewrites nothing (same inode: no temp+rename).
	after, err := os.Lstat(gcLink)
	if err != nil {
		t.Fatalf("lstat gc link after: %v", err)
	}
	if !os.SameFile(before, after) {
		t.Fatalf("gc symlink replaced on a converged refresh")
	}

	// No temp-symlink debris left behind.
	entries, err := os.ReadDir(cityBdShimbinDir(cityPath))
	if err != nil {
		t.Fatalf("reading shim bin dir: %v", err)
	}
	for _, e := range entries {
		if name := e.Name(); name != "gc" && name != "bd" {
			t.Fatalf("unexpected entry in shim bin dir: %q", name)
		}
	}
}

func TestEnsureCityBdShimbinNoBdOnPATHSkipsBdSymlink(t *testing.T) {
	cityPath := t.TempDir()
	emptyDir := t.TempDir() // a PATH entry with no bd
	t.Setenv("PATH", emptyDir)

	if err := ensureCityBdShimbin(cityPath, io.Discard); err != nil {
		t.Fatalf("ensureCityBdShimbin: %v", err)
	}

	if !isSymlink(cityBdShimbinGCPath(cityPath)) {
		t.Fatalf("gc symlink should still be created without a real bd")
	}
	bdLink := filepath.Join(cityBdShimbinDir(cityPath), "bd")
	if _, err := os.Lstat(bdLink); !os.IsNotExist(err) {
		t.Fatalf("bd symlink should be skipped when no real bd on PATH (lstat err=%v)", err)
	}
}

func TestSessionGCBinPointsIntoShimbinWhenInstalled(t *testing.T) {
	cityPath := t.TempDir()
	realBdDir := t.TempDir()
	writeFakeBd(t, realBdDir)
	t.Setenv("PATH", realBdDir)
	if err := ensureCityBdShimbin(cityPath, io.Discard); err != nil {
		t.Fatalf("install: %v", err)
	}

	gcBin := sessionGCBinForCity(cityPath, map[string]string{})
	if gcBin != cityBdShimbinGCPath(cityPath) {
		t.Fatalf("GC_BIN = %q, want shimbin gc %q", gcBin, cityBdShimbinGCPath(cityPath))
	}
	// The dir prependGCBinDirToPATH fronts must be the shim bin dir, so the
	// sibling `bd` symlink wins on a session PATH.
	if filepath.Dir(gcBin) != cityBdShimbinDir(cityPath) {
		t.Fatalf("GC_BIN dir = %q, want shim bin dir %q", filepath.Dir(gcBin), cityBdShimbinDir(cityPath))
	}
}

func TestSessionGCBinFallsBackWhenNotInstalled(t *testing.T) {
	cityPath := t.TempDir() // no shim bin dir installed
	env := map[string]string{}
	gcBin := sessionGCBinForCity(cityPath, env)
	exe, _ := os.Executable()
	if gcBin != exe {
		t.Fatalf("GC_BIN = %q, want os.Executable fallback %q", gcBin, exe)
	}
	if _, set := env[realBdEnvVar]; set {
		t.Fatalf("GC_BD_REAL must not be set when the shim is not installed")
	}
}

// TestGCBINDerivationFromCityPathNotOsExecutable locks the copy-free recursion
// fix: GC_BIN is the cityPath-derived shimbin path, not os.Executable() (the
// symlink target), so a respawned controller cannot lose the redirect.
func TestGCBINDerivationFromCityPathNotOsExecutable(t *testing.T) {
	cityPath := t.TempDir()
	realBdDir := t.TempDir()
	writeFakeBd(t, realBdDir)
	t.Setenv("PATH", realBdDir)
	if err := ensureCityBdShimbin(cityPath, io.Discard); err != nil {
		t.Fatalf("install: %v", err)
	}

	gcBin := sessionGCBinForCity(cityPath, map[string]string{})
	if exe, _ := os.Executable(); gcBin == exe {
		t.Fatalf("GC_BIN resolved to os.Executable() %q; must be the cityPath-derived shimbin path", exe)
	}
	if gcBin != cityBdShimbinGCPath(cityPath) {
		t.Fatalf("GC_BIN = %q, want %q", gcBin, cityBdShimbinGCPath(cityPath))
	}
}

// TestSessionEnvSetsGCBDRealToRealBdNotShim proves GC_BD_REAL resolves the real
// bd even when the resolving process's own PATH is fronted with the shim bin dir
// (the controller case), so the shim's passthrough never recurses.
func TestSessionEnvSetsGCBDRealToRealBdNotShim(t *testing.T) {
	cityPath := t.TempDir()
	realBdDir := t.TempDir()
	realBd := writeFakeBd(t, realBdDir)
	t.Setenv("PATH", realBdDir)
	if err := ensureCityBdShimbin(cityPath, io.Discard); err != nil {
		t.Fatalf("install: %v", err)
	}

	// Simulate a controller whose PATH is fronted with the shim bin dir.
	t.Setenv("PATH", cityBdShimbinDir(cityPath)+string(os.PathListSeparator)+realBdDir)
	env := map[string]string{}
	_ = sessionGCBinForCity(cityPath, env)

	if env[realBdEnvVar] != realBd {
		t.Fatalf("GC_BD_REAL = %q, want real bd %q", env[realBdEnvVar], realBd)
	}
	if strings.HasPrefix(env[realBdEnvVar], cityBdShimbinDir(cityPath)) {
		t.Fatalf("GC_BD_REAL %q points into the shim bin dir (recursion)", env[realBdEnvVar])
	}
}

func TestResolveRealBdExcludingDirSkipsShimbin(t *testing.T) {
	cityPath := t.TempDir()
	shimbin := cityBdShimbinDir(cityPath)
	// A `bd` inside the shim bin dir (the recursive trap) must be skipped...
	writeFakeBd(t, shimbin)
	// ...in favor of the real bd in a later PATH entry.
	realBdDir := t.TempDir()
	realBd := writeFakeBd(t, realBdDir)
	t.Setenv("PATH", shimbin+string(os.PathListSeparator)+realBdDir)

	got, err := resolveRealBdExcludingDir(shimbin)
	if err != nil {
		t.Fatalf("resolveRealBdExcludingDir: %v", err)
	}
	if got != realBd {
		t.Fatalf("resolved bd = %q, want the real bd %q (not the shimbin one)", got, realBd)
	}
}
