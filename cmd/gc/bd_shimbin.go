package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/citylayout"
)

// bdShimbinDirName is the per-city directory, under the runtime root, that holds
// the gc-as-bd shim symlinks placed on every managed worker session's PATH.
const bdShimbinDirName = "shimbin"

// cityBdShimbinDir returns the per-city directory that holds the gc/bd shim
// symlinks for cityPath (<cityPath>/.gc/shimbin).
func cityBdShimbinDir(cityPath string) string {
	return filepath.Join(cityPath, citylayout.RuntimeRoot, bdShimbinDirName)
}

// cityBdShimbinGCPath returns the path of the `gc` symlink inside the city's
// shim bin dir. This is the GC_BIN value handed to managed sessions: its
// directory (the shim bin dir) is fronted on PATH by prependGCBinDirToPATH, so a
// sibling `bd` symlink in the same dir resolves to gc-invoked-as-bd.
func cityBdShimbinGCPath(cityPath string) string {
	return filepath.Join(cityBdShimbinDir(cityPath), "gc")
}

// ensureCityBdShimbin installs the gc-as-bd shim for cityPath's managed worker
// sessions. It (re)creates <cityPath>/.gc/shimbin/gc and .../bd as symlinks to
// the running gc binary, so a worker whose PATH is fronted with the shim bin dir
// resolves `bd` to gc-invoked-as-bd and routes bead ops through the controller.
//
// Installing it for every managed city is safe regardless of graph_store: the
// shim adapts per-city at runtime (classifyBdShimVerb), routing by-id verbs in
// all cities and only refusing graph verbs when graph_store=sqlite. By-id
// mediation keeps the controller's cache authoritative even in Dolt-only cities.
//
// Symlinks (not a copy of gc) suffice because session GC_BIN is computed from
// cityPath, not re-derived from os.Executable() — see sessionGCBinForCity — so a
// respawned controller never loses the redirect. All writes are confined to
// <cityPath>/.gc/shimbin; the real gc/bd install dir is only read, never
// written, so the user's real `bd` is never clobbered.
//
// When no real bd is found on PATH the `bd` symlink is skipped (not an error):
// a shimmed `bd` with no passthrough target would refuse loudly, so leaving
// `bd` unshimmed preserves the no-bd-installed behavior. Errors creating the
// dir or the gc symlink are returned for the caller to log non-fatally; on
// error sessions stay on the real gc (no shim), matching pre-install behavior.
func ensureCityBdShimbin(cityPath string, stderr io.Writer) error {
	gcExe, err := os.Executable()
	if err != nil || gcExe == "" {
		return fmt.Errorf("resolving gc binary: %w", err)
	}
	dir := cityBdShimbinDir(cityPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating shim bin dir %q: %w", dir, err)
	}
	if err := atomicSymlinkShimbin(gcExe, cityBdShimbinGCPath(cityPath)); err != nil {
		return fmt.Errorf("linking gc shim: %w", err)
	}
	// The `bd` symlink targets the in-dir gc symlink (not the real bd): a worker
	// invoking `bd` execs gc, whose "bd" argv0 basename activates the shim. Only
	// create it when a real bd exists to pass through to.
	if _, err := resolveRealBdExcludingDir(dir); err != nil {
		fmt.Fprintf(stderr, "gc supervisor: bd shim install: no real bd on PATH; worker bd redirect disabled (%v)\n", err) //nolint:errcheck
		return nil
	}
	if err := atomicSymlinkShimbin(cityBdShimbinGCPath(cityPath), filepath.Join(dir, "bd")); err != nil {
		return fmt.Errorf("linking bd shim: %w", err)
	}
	return nil
}

// sessionGCBinForCity returns the GC_BIN value for a managed session in cityPath
// and, when the bd redirect is installed, sets GC_BD_REAL in agentEnv.
//
// When the city's shim bin dir is installed (the gc symlink exists), GC_BIN is
// the shimbin gc symlink: prependGCBinDirToPATH fronts its directory, so the
// sibling `bd` symlink wins and a worker's `bd` routes through the controller.
// The value is derived from cityPath, never from os.Executable(), so a respawned
// controller recomputes the same redirect for its grandchild sessions without a
// gc copy. When the bd symlink is present, GC_BD_REAL is resolved to the real bd
// (excluding the shim bin dir) so the shim's passthrough never recurses, even in
// a controller whose own PATH is already fronted with the shim bin dir.
//
// When no shim bin dir is installed it returns the running gc binary
// (os.Executable), preserving the pre-install behavior, and sets nothing.
func sessionGCBinForCity(cityPath string, agentEnv map[string]string) string {
	gcLink := cityBdShimbinGCPath(cityPath)
	if !isSymlink(gcLink) {
		if exe, err := os.Executable(); err == nil && exe != "" {
			return exe
		}
		return ""
	}
	dir := cityBdShimbinDir(cityPath)
	if isSymlink(filepath.Join(dir, "bd")) {
		if realBd, err := resolveRealBdExcludingDir(dir); err == nil {
			agentEnv[realBdEnvVar] = realBd
		}
	}
	return gcLink
}

// resolveRealBdExcludingDir finds the absolute path of the real bd binary to use
// as the shim's GC_BD_REAL passthrough target by scanning PATH and skipping
// excludeDir (the shim bin dir). Skipping that dir makes resolution recursion-
// safe in any process — including a controller whose own PATH is already fronted
// with the shim bin dir — because the original PATH entry holding the real bd is
// preserved behind the prepended shim dir.
func resolveRealBdExcludingDir(excludeDir string) (string, error) {
	excludeClean := filepath.Clean(excludeDir)
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" || filepath.Clean(dir) == excludeClean {
			continue
		}
		candidate := filepath.Join(dir, "bd")
		if !isExecutableFile(candidate) {
			continue
		}
		abs, err := filepath.Abs(candidate)
		if err != nil {
			continue
		}
		return abs, nil
	}
	return "", fmt.Errorf("no executable bd found on PATH outside %s", excludeDir)
}

// isExecutableFile reports whether path is a regular (symlinks followed) file
// with an execute bit set.
func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	return info.Mode().Perm()&0o111 != 0
}

// isSymlink reports whether path is a symlink (not following it).
func isSymlink(path string) bool {
	info, err := os.Lstat(path)
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeSymlink != 0
}

// atomicSymlinkShimbin points path at target via a temp-then-rename, and is a
// no-op when the link already resolves to target (so a converged refresh creates
// nothing). POSIX rename(2) is atomic, so a concurrent reader never observes a
// missing or partially-written link during replacement.
func atomicSymlinkShimbin(target, path string) error {
	if existing, err := os.Readlink(path); err == nil && existing == target {
		return nil
	}
	dir := filepath.Dir(path)
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fmt.Errorf("allocating temp symlink nonce: %w", err)
	}
	tmp := filepath.Join(dir, "."+filepath.Base(path)+".tmp."+hex.EncodeToString(nonce[:]))
	if err := os.Symlink(target, tmp); err != nil {
		return fmt.Errorf("creating temp symlink %q: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("renaming temp symlink into place: %w", err)
	}
	return nil
}
