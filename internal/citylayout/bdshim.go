package citylayout

import (
	"fmt"
	"os"
	"path/filepath"
)

// RealBdEnvVar is the environment variable that tells the gc-as-bd shim where
// the real bd binary is, so a shimmed `bd` invocation passes through to it
// instead of refusing. It must be injected into every environment whose PATH may
// resolve `bd` to the shim — managed worker sessions AND order/gate condition
// scripts. Session exec already sets it (see cmd/gc sessionGCBinForCity); this
// centralizes the resolver so order/gate exec can set it identically.
const RealBdEnvVar = "GC_BD_REAL"

// bdShimbinDirName is the per-city runtime subdirectory holding the gc/bd shim
// symlinks fronted on a managed worker's PATH.
const bdShimbinDirName = "shimbin"

// ShimbinDir returns the per-city gc/bd shim bin directory
// (<cityPath>/.gc/shimbin).
func ShimbinDir(cityPath string) string {
	return filepath.Join(cityPath, RuntimeRoot, bdShimbinDirName)
}

// ShimbinGCPath returns the gc shim symlink path whose presence signals the
// bd-shim is installed for cityPath (<cityPath>/.gc/shimbin/gc).
func ShimbinGCPath(cityPath string) string {
	return filepath.Join(ShimbinDir(cityPath), "gc")
}

// ShimInstalled reports whether the bd-shim is installed for cityPath (the gc
// shim symlink exists). It is naturally false under bd_shim=off, because the
// supervisor removes the shim dir, so callers front the shim on PATH without
// threading the bd_shim config: an absent shim just no-ops.
func ShimInstalled(cityPath string) bool {
	info, err := os.Lstat(ShimbinGCPath(cityPath))
	return err == nil && info.Mode()&os.ModeSymlink != 0
}

// ResolveRealBd resolves the real bd binary for cityPath by scanning PATH while
// skipping the city's shim bin dir, so a gate/order condition script whose `bd`
// resolves to the shim has a passthrough target. Returns an error when no real
// bd is found outside the shim dir.
func ResolveRealBd(cityPath string) (string, error) {
	return ResolveRealBdExcludingDir(ShimbinDir(cityPath))
}

// ResolveRealBdExcludingDir finds the absolute path of the real bd binary by
// scanning PATH and skipping excludeDir (the shim bin dir). Skipping that dir
// makes resolution recursion-safe even in a process whose own PATH is already
// fronted with the shim bin dir, because the original PATH entry holding the
// real bd is preserved behind the prepended shim dir.
func ResolveRealBdExcludingDir(excludeDir string) (string, error) {
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
