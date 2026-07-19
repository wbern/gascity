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

// bdproxyBinName is the file name of the tiny bd thin client installed beside the
// gc binary. When present, the `bd` shim symlink targets it so a worker's bd call
// skips gc's ~200ms cold-start; when absent, the symlink targets the real bd
// directly (the traditional, shim-free path). gc itself is never a bd shim.
const bdproxyBinName = "bdproxy"

// bdproxyBesideGC returns the absolute path of the bdproxy binary installed in the
// same directory as the running gc binary (following the gc symlink), or "" when
// no executable bdproxy is found there. Both the invoked path and its symlink-
// resolved path are checked so a gc started via a symlink (e.g. .local/bin/gc ->
// go/bin/gc) still finds a bdproxy installed beside either.
func bdproxyBesideGC() string {
	exe, err := os.Executable()
	if err != nil || exe == "" {
		return ""
	}
	return bdproxyBesideExe(exe)
}

// bdproxyBesideExe returns the absolute path of an executable bdproxy in the same
// directory as exe (or as exe's symlink-resolved target), or "" when none is
// found. Split from bdproxyBesideGC so the resolution is testable without mocking
// os.Executable.
func bdproxyBesideExe(exe string) string {
	candidates := []string{filepath.Join(filepath.Dir(exe), bdproxyBinName)}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil && resolved != exe {
		candidates = append(candidates, filepath.Join(filepath.Dir(resolved), bdproxyBinName))
	}
	for _, cand := range candidates {
		if !isExecutableFile(cand) {
			continue
		}
		if abs, err := filepath.Abs(cand); err == nil {
			return abs
		}
	}
	return ""
}

// cityBdShimbinGCPath returns the path of the `gc` symlink inside the city's
// shim bin dir. This is the GC_BIN value handed to managed sessions: its
// directory (the shim bin dir) is fronted on PATH by prependGCBinDirToPATH, so a
// sibling `bd` symlink in the same dir resolves to gc-invoked-as-bd.
func cityBdShimbinGCPath(cityPath string) string {
	return filepath.Join(cityBdShimbinDir(cityPath), "gc")
}

// bdShimZdotdirName is the per-city directory, under the runtime root, holding a
// gc-managed ZDOTDIR for managed worker sessions. It makes the shim bin dir win
// on PATH in the agent's zsh even when the user's rc re-prepends a real-bd dir.
const bdShimZdotdirName = "shimzdotdir"

// cityBdShimZdotdir returns the per-city gc-managed ZDOTDIR
// (<cityPath>/.gc/shimzdotdir).
func cityBdShimZdotdir(cityPath string) string {
	return filepath.Join(cityPath, citylayout.RuntimeRoot, bdShimZdotdirName)
}

// ensureCityBdShimZdotdir writes a gc-managed ZDOTDIR for cityPath so that `bd`
// resolves to the shim in an agent's zsh even when the user profile re-prepends a
// real-bd directory (e.g. ~/.zshrc adding ~/go/bin ahead of the shim bin dir,
// which otherwise buries the shim bin dir the process PATH was fronted with).
//
// Each rc file sources the user's real equivalent first (preserving the agent's
// shell environment), and .zshrc then fronts the shim bin dir on PATH LAST — so
// the redirect wins in both the pane's login shell and the agent's interactive
// tool shell, both of which re-run the user rc. sessionGCBinForCity sets ZDOTDIR
// to this directory only when the bd redirect is active. Errors are returned for
// the caller to log non-fatally; on error sessions fall back to the user's rc
// (no PATH-front guarantee) but are otherwise unaffected.
func ensureCityBdShimZdotdir(cityPath string) error {
	dir := cityBdShimZdotdir(cityPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating shim zdotdir %q: %w", dir, err)
	}
	shimbin := cityBdShimbinDir(cityPath)
	// front prepends the shim bin dir on PATH so `bd` routes through the
	// controller. The idempotent guard (only prepend when it isn't already first)
	// avoids unbounded PATH growth across nested subshells. It MUST live in
	// .zshenv because the agent tool shell runs a NON-interactive `zsh -c`, which
	// sources .zshenv but NOT .zshrc; it is repeated in .zshrc so it also wins in
	// an interactive shell after the user rc re-prepends a real-bd dir (~/go/bin).
	front := "# Front the gc-as-bd shim bin dir so `bd` routes through the controller.\n" +
		"if [ \"${PATH%%:*}\" != \"" + shimbin + "\" ]; then export PATH=\"" + shimbin + ":$PATH\"; fi\n"
	files := map[string]string{
		".zshenv":   "[ -f \"$HOME/.zshenv\" ] && source \"$HOME/.zshenv\"\n" + front,
		".zprofile": "[ -f \"$HOME/.zprofile\" ] && source \"$HOME/.zprofile\"\n",
		".zlogin":   "[ -f \"$HOME/.zlogin\" ] && source \"$HOME/.zlogin\"\n",
		".zshrc":    "[ -f \"$HOME/.zshrc\" ] && source \"$HOME/.zshrc\"\n" + front,
	}
	for name, content := range files {
		if err := atomicWriteFile(filepath.Join(dir, name), []byte(content)); err != nil {
			return fmt.Errorf("writing shim zdotdir %s: %w", name, err)
		}
	}
	return nil
}

// ensureCityBdShimbin installs the gc-as-bd shim for cityPath's managed worker
// sessions. It (re)creates <cityPath>/.gc/shimbin/gc and .../bd as symlinks to
// the running gc binary. The sibling `bd` symlink points at the tiny bdproxy
// thin client when it is installed beside gc (routing hot bead ops through the
// warm controller), else at the real bd directly — gc itself is never a bd shim.
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
	// The `bd` symlink targets the tiny bdproxy thin client when it is installed
	// beside the gc binary (the fast path — no 117MB gc cold-start per bd call),
	// else the real bd directly (the traditional, shim-free path). It never
	// targets gc: gc is no longer a bd shim. Only create it when a real bd exists.
	realBd, err := resolveRealBdExcludingDir(dir)
	if err != nil {
		fmt.Fprintf(stderr, "gc supervisor: bd shim install: no real bd on PATH; worker bd redirect disabled (%v)\n", err) //nolint:errcheck
		return nil
	}
	bdTarget := realBd
	if bp := bdproxyBesideGC(); bp != "" {
		bdTarget = bp
	}
	if err := atomicSymlinkShimbin(bdTarget, filepath.Join(dir, "bd")); err != nil {
		return fmt.Errorf("linking bd shim: %w", err)
	}
	// With the bd redirect active, install the gc-managed ZDOTDIR so a worker's
	// zsh fronts the shim bin dir even when the user rc re-prepends a real-bd dir.
	if err := ensureCityBdShimZdotdir(cityPath); err != nil {
		return fmt.Errorf("installing shim zdotdir: %w", err)
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
			agentEnv[citylayout.RealBdEnvVar] = realBd
		}
		// Point the worker's zsh at the gc-managed ZDOTDIR so the shim bin dir
		// wins on PATH even after the user rc re-prepends a real-bd dir. Only set
		// when the ZDOTDIR is actually present, so a partial install never breaks
		// the agent's shell startup.
		if zdotdir := cityBdShimZdotdir(cityPath); isDir(zdotdir) {
			agentEnv["ZDOTDIR"] = zdotdir
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

// isDir reports whether path is a directory (symlinks followed).
func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
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
