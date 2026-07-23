package main

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode"
)

// earlyBdShimPath resolves the city-managed bd entry only after proving that
// it targets the standalone bdshim companion installed beside gc. It
// deliberately never falls back to PATH: an enabled fast path must not
// execute an unrelated or stale bdshim binary, and it must retain the managed
// entrypoint's argv[0] behavior.
var earlyBdShimPath = func(cityPath string) (string, error) {
	if gcPath, err := os.Executable(); err == nil && gcPath != "" {
		if shimPath := earlyBdShimForCity(gcPath, cityPath); shimPath != "" {
			return shimPath, nil
		}
	}
	return "", exec.ErrNotFound
}

// earlyBdShimBesideGC finds an executable bdshim beside the supplied gc path
// or the target when gc is a symlink. Keeping this lookup separate makes the
// installed-binary trust boundary testable without mocking os.Executable.
func earlyBdShimBesideGC(gcPath string) string {
	candidates := []string{filepath.Join(filepath.Dir(gcPath), "bdshim")}
	if resolved, err := filepath.EvalSymlinks(gcPath); err == nil && resolved != gcPath {
		candidates = append(candidates, filepath.Join(filepath.Dir(resolved), "bdshim"))
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0 {
			return candidate
		}
	}
	return ""
}

// earlyBdShimForCity returns the managed `bd` entry only when it resolves to
// the executable companion trusted by gc. Calling the companion directly as
// `bdshim` is observably different from calling the managed entry as `bd`:
// bdshim uses argv[0] as part of its compatibility/routing contract. Requiring
// identical resolved files preserves that contract without trusting city PATH.
func earlyBdShimForCity(gcPath, cityPath string) string {
	trustedPath := earlyBdShimBesideGC(gcPath)
	trustedResolved, ok := resolvedExecutablePath(trustedPath)
	if !ok {
		return ""
	}
	managedPath := filepath.Join(cityPath, ".gc", "shimbin", "bd")
	managedResolved, ok := resolvedExecutablePath(managedPath)
	if !ok || managedResolved != trustedResolved {
		return ""
	}
	return managedPath
}

func resolvedExecutablePath(path string) (string, bool) {
	if path == "" {
		return "", false
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false
	}
	info, err := os.Stat(resolved)
	if err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		return "", false
	}
	return filepath.Clean(resolved), true
}

// bdFastpathEnabled defaults the proven read-only fast path on for this fork.
// Operators can set GC_BD_FASTPATH=0 to force the ordinary gc bd path. Unknown
// values fail closed to that ordinary path instead of enabling unexpectedly.
func bdFastpathEnabled(raw string) bool {
	switch strings.TrimSpace(raw) {
	case "", "1":
		return true
	default:
		return false
	}
}

// tryEarlyBdShim handles the deliberately tiny read-only part of the
// gc bd surface that has byte-level controller-rendering parity today. It is
// called before telemetry, Cobra construction, and eager pack discovery, which
// are the dominant cost of a fresh gc bd invocation.
//
// Every shape outside this allowlist returns handled=false and therefore takes
// the existing doBd path. In particular, it never bypasses rig resolution,
// mutation guards, heartbeat rewriting, or the work-record close gate.
func tryEarlyBdShim(args []string, stdin io.Reader, stdout, stderr io.Writer) (code int, handled bool) {
	if !bdFastpathEnabled(os.Getenv("GC_BD_FASTPATH")) {
		return 0, false
	}
	cityPath := managedCityPath()
	if cityPath == "" {
		return 0, false
	}
	bdArgs, ok := earlyBdShimShowArgs(args)
	if !ok {
		return 0, false
	}
	shimPath, err := earlyBdShimPath(cityPath)
	if err != nil || shimPath == "" {
		return 0, false
	}

	cmd := exec.Command(shimPath, bdArgs...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if exitCode := exitErr.ExitCode(); exitCode > 0 {
				return exitCode, true
			}
		}
		// The shim was selected successfully. Do not fall through to raw bd on
		// a launch error: a routed controller read must fail loudly rather than
		// risk reading the caller's unrelated cwd store.
		return 1, true
	}
	return 0, true
}

// hasManagedCityContext matches the standalone bdshim's city-resolution
// contract. Without it, the shim may legitimately pass through to raw bd; gc
// bd must instead retain its full city/rig discovery semantics.
func managedCityPath() string {
	if cityPath := strings.TrimSpace(os.Getenv("GC_CITY_PATH")); cityPath != "" {
		return cityPath
	}
	// GC_CITY may be a city name, which needs full config resolution. Only an
	// absolute path is enough context for this pre-Cobra fast path.
	if cityPath := strings.TrimSpace(os.Getenv("GC_CITY")); filepath.IsAbs(cityPath) {
		return cityPath
	}
	return ""
}

// earlyBdShimShowArgs accepts only `gc bd show <id> --json`. The controller
// resolves a bead ID across the city's stores, while list/ready depend on rig
// scope and mutations depend on gc bd's guards. Restricting this first slice to
// a JSON point lookup makes the fast path auditable and safely expandable.
func earlyBdShimShowArgs(args []string) ([]string, bool) {
	if len(args) != 4 || args[0] != "bd" || args[1] != "show" || args[3] != "--json" {
		return nil, false
	}
	rawID := args[2]
	id := strings.TrimSpace(rawID)
	if id == "" || id != rawID || strings.HasPrefix(id, "-") || strings.IndexFunc(id, unicode.IsSpace) >= 0 {
		return nil, false
	}
	return []string{"show", id, "--json"}, true
}
