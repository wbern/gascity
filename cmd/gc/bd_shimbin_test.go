package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
)

// TestEnsureCityBdShimbinOffSkipsRedirect pins bd_shim=off: no redirect is
// installed, so a worker's `bd`/`gc` resolve to the real binaries on PATH (the
// clean baseline for benchmarking / opt-out).
func TestEnsureCityBdShimbinOffSkipsRedirect(t *testing.T) {
	cityPath := t.TempDir()
	realBdDir := t.TempDir()
	writeFakeBd(t, realBdDir)
	t.Setenv("PATH", realBdDir)

	if err := ensureCityBdShimbin(cityPath, config.BdShimModeOff, io.Discard); err != nil {
		t.Fatalf("ensureCityBdShimbin(off): %v", err)
	}
	if isSymlink(cityBdShimbinGCPath(cityPath)) {
		t.Fatalf("gc symlink must NOT be installed under bd_shim=off")
	}
	if isSymlink(filepath.Join(cityBdShimbinDir(cityPath), "bd")) {
		t.Fatalf("bd symlink must NOT be installed under bd_shim=off")
	}
	// sessionGCBinForCity falls back to the real gc, setting no redirect env.
	env := map[string]string{}
	if gcBin := sessionGCBinForCity(cityPath, env); gcBin != mustExe(t) {
		t.Fatalf("GC_BIN = %q, want os.Executable fallback under off", gcBin)
	}
	if _, set := env[citylayout.RealBdEnvVar]; set {
		t.Fatalf("GC_BD_REAL must not be set under bd_shim=off")
	}
}

// TestEnsureCityBdShimbinOffRemovesStaleRedirect pins that switching to off
// tears down a prior auto/on install (idempotent opt-out), so a bounce into off
// leaves no dangling shimbin/bd that would keep routing.
func TestEnsureCityBdShimbinOffRemovesStaleRedirect(t *testing.T) {
	cityPath := t.TempDir()
	realBdDir := t.TempDir()
	writeFakeBd(t, realBdDir)
	t.Setenv("PATH", realBdDir)

	if err := ensureCityBdShimbin(cityPath, config.BdShimModeAuto, io.Discard); err != nil {
		t.Fatalf("install (auto): %v", err)
	}
	if !isSymlink(filepath.Join(cityBdShimbinDir(cityPath), "bd")) {
		t.Fatalf("precondition: bd symlink should exist after auto install")
	}
	if err := ensureCityBdShimbin(cityPath, config.BdShimModeOff, io.Discard); err != nil {
		t.Fatalf("switch to off: %v", err)
	}
	if _, err := os.Lstat(cityBdShimbinDir(cityPath)); !os.IsNotExist(err) {
		t.Fatalf("shim bin dir should be removed under off (lstat err=%v)", err)
	}
}

// TestEnsureCityBdShimbinOnWarnsWhenBdproxyMissing pins bd_shim=on with no
// bdproxy beside gc: it still installs the redirect (passthrough to real bd) but
// warns loudly rather than silently degrading to auto.
func TestEnsureCityBdShimbinOnWarnsWhenBdproxyMissing(t *testing.T) {
	cityPath := t.TempDir()
	realBdDir := t.TempDir()
	writeFakeBd(t, realBdDir)
	t.Setenv("PATH", realBdDir)

	var stderr strings.Builder
	if err := ensureCityBdShimbin(cityPath, config.BdShimModeOn, &stderr); err != nil {
		t.Fatalf("ensureCityBdShimbin(on): %v", err)
	}
	if !strings.Contains(stderr.String(), "bd_shim=on but no bdproxy") {
		t.Fatalf("want on-missing warning, got stderr: %q", stderr.String())
	}
	if !isSymlink(filepath.Join(cityBdShimbinDir(cityPath), "bd")) {
		t.Fatalf("bd symlink should still be installed under on (passthrough fallback)")
	}
}

func mustExe(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return exe
}

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

	if err := ensureCityBdShimbin(cityPath, config.BdShimModeAuto, io.Discard); err != nil {
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
	// bd -> the real bd directly (gc is no longer a bd shim; no bdproxy in this test).
	wantBd, err := resolveRealBdExcludingDir(cityBdShimbinDir(cityPath))
	if err != nil {
		t.Fatalf("resolve real bd: %v", err)
	}
	if got, _ := os.Readlink(bdLink); got != wantBd {
		t.Fatalf("bd symlink -> %q, want real bd %q", got, wantBd)
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

func TestBdproxyBesideExe(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "gc")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// No bdproxy beside exe yet.
	if got := bdproxyBesideExe(exe); got != "" {
		t.Fatalf("no bdproxy: got %q, want empty", got)
	}
	// An executable bdproxy beside exe is found.
	bp := filepath.Join(dir, "bdproxy")
	if err := os.WriteFile(bp, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := bdproxyBesideExe(exe); got != bp {
		t.Fatalf("bdproxy beside exe: got %q, want %q", got, bp)
	}
	// A non-executable bdproxy is ignored (not a valid shim target).
	if err := os.Chmod(bp, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := bdproxyBesideExe(exe); got != "" {
		t.Fatalf("non-exec bdproxy: got %q, want empty", got)
	}
}

// TestBdproxyBesideExeFollowsSymlink verifies a gc started via a symlink still
// finds a bdproxy installed beside the symlink's real target.
func TestBdproxyBesideExeFollowsSymlink(t *testing.T) {
	realDir := t.TempDir()
	realExe := filepath.Join(realDir, "gc")
	if err := os.WriteFile(realExe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	bp := filepath.Join(realDir, "bdproxy")
	if err := os.WriteFile(bp, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	linkDir := t.TempDir()
	linkExe := filepath.Join(linkDir, "gc")
	if err := os.Symlink(realExe, linkExe); err != nil {
		t.Fatal(err)
	}
	// bdproxy is beside the symlink target, not beside the symlink itself.
	// Normalize via EvalSymlinks: on macOS the tmp root (/var) is itself a symlink
	// to /private/var, which the resolver follows.
	wantBP, err := filepath.EvalSymlinks(bp)
	if err != nil {
		t.Fatal(err)
	}
	if got := bdproxyBesideExe(linkExe); got != wantBP {
		t.Fatalf("symlink-resolved bdproxy: got %q, want %q", got, wantBP)
	}
}

func TestEnsureCityBdShimbinIdempotentAndAtomic(t *testing.T) {
	cityPath := t.TempDir()
	realBdDir := t.TempDir()
	writeFakeBd(t, realBdDir)
	t.Setenv("PATH", realBdDir)

	if err := ensureCityBdShimbin(cityPath, config.BdShimModeAuto, io.Discard); err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	gcLink := cityBdShimbinGCPath(cityPath)
	before, err := os.Lstat(gcLink)
	if err != nil {
		t.Fatalf("lstat gc link: %v", err)
	}

	if err := ensureCityBdShimbin(cityPath, config.BdShimModeAuto, io.Discard); err != nil {
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

	if err := ensureCityBdShimbin(cityPath, config.BdShimModeAuto, io.Discard); err != nil {
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
	if err := ensureCityBdShimbin(cityPath, config.BdShimModeAuto, io.Discard); err != nil {
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
	if _, set := env[citylayout.RealBdEnvVar]; set {
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
	if err := ensureCityBdShimbin(cityPath, config.BdShimModeAuto, io.Discard); err != nil {
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
	if err := ensureCityBdShimbin(cityPath, config.BdShimModeAuto, io.Discard); err != nil {
		t.Fatalf("install: %v", err)
	}

	// Simulate a controller whose PATH is fronted with the shim bin dir.
	t.Setenv("PATH", cityBdShimbinDir(cityPath)+string(os.PathListSeparator)+realBdDir)
	env := map[string]string{}
	_ = sessionGCBinForCity(cityPath, env)

	if env[citylayout.RealBdEnvVar] != realBd {
		t.Fatalf("GC_BD_REAL = %q, want real bd %q", env[citylayout.RealBdEnvVar], realBd)
	}
	if strings.HasPrefix(env[citylayout.RealBdEnvVar], cityBdShimbinDir(cityPath)) {
		t.Fatalf("GC_BD_REAL %q points into the shim bin dir (recursion)", env[citylayout.RealBdEnvVar])
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

// TestEnsureCityBdShimbinInstallsZdotdir verifies the install writes the
// gc-managed ZDOTDIR whose .zshrc sources the user's rc then fronts the shim bin
// dir on PATH — the mechanism that makes `bd` win in the agent's zsh even when
// the user rc re-prepends a real-bd dir (gcw-tymu).
func TestEnsureCityBdShimbinInstallsZdotdir(t *testing.T) {
	cityPath := t.TempDir()
	realBdDir := t.TempDir()
	writeFakeBd(t, realBdDir)
	t.Setenv("PATH", realBdDir)

	if err := ensureCityBdShimbin(cityPath, config.BdShimModeAuto, io.Discard); err != nil {
		t.Fatalf("ensureCityBdShimbin: %v", err)
	}

	zdir := cityBdShimZdotdir(cityPath)
	if !isDir(zdir) {
		t.Fatalf("shim zdotdir %q not created", zdir)
	}
	for _, f := range []string{".zshenv", ".zprofile", ".zshrc", ".zlogin"} {
		if _, err := os.Stat(filepath.Join(zdir, f)); err != nil {
			t.Fatalf("zdotdir missing %s: %v", f, err)
		}
	}
	shimbin := cityBdShimbinDir(cityPath)
	front := `export PATH="` + shimbin + `:$PATH"`
	// The shim bin dir must be fronted AFTER the user rc is sourced (so it wins),
	// in BOTH .zshenv (the agent tool shell is a non-interactive `zsh -c`, which
	// runs .zshenv but NOT .zshrc) and .zshrc (interactive shells).
	for rc, userSrc := range map[string]string{
		".zshenv": `source "$HOME/.zshenv"`,
		".zshrc":  `source "$HOME/.zshrc"`,
	} {
		data, err := os.ReadFile(filepath.Join(zdir, rc))
		if err != nil {
			t.Fatalf("reading %s: %v", rc, err)
		}
		body := string(data)
		if !strings.Contains(body, userSrc) {
			t.Fatalf("%s does not source the user rc:\n%s", rc, body)
		}
		if !strings.Contains(body, front) {
			t.Fatalf("%s does not front the shim bin dir (want %q):\n%s", rc, front, body)
		}
		if strings.Index(body, userSrc) > strings.Index(body, front) {
			t.Fatalf("%s fronts shim bin dir BEFORE sourcing user rc (order wrong):\n%s", rc, body)
		}
	}
}

// TestSessionGCBinSetsZdotdirWhenInstalled verifies a managed session's env gets
// ZDOTDIR pointing at the gc-managed dir when the bd redirect is active.
func TestSessionGCBinSetsZdotdirWhenInstalled(t *testing.T) {
	cityPath := t.TempDir()
	realBdDir := t.TempDir()
	writeFakeBd(t, realBdDir)
	t.Setenv("PATH", realBdDir)

	if err := ensureCityBdShimbin(cityPath, config.BdShimModeAuto, io.Discard); err != nil {
		t.Fatalf("ensureCityBdShimbin: %v", err)
	}
	env := map[string]string{}
	sessionGCBinForCity(cityPath, env)
	if env["ZDOTDIR"] != cityBdShimZdotdir(cityPath) {
		t.Fatalf("ZDOTDIR = %q, want %q", env["ZDOTDIR"], cityBdShimZdotdir(cityPath))
	}
}
