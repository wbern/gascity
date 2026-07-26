package main

import (
	"errors"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/gastownhall/gascity/internal/bddispatch"
	"github.com/gastownhall/gascity/internal/bdexperiment"
	"github.com/gastownhall/gascity/internal/bdroute"
	"github.com/gastownhall/gascity/internal/bdshim"
	"github.com/gastownhall/gascity/internal/beadclient"
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

var earlyBdLookPath = exec.LookPath

var earlyBdExperimentNext = rand.IntN

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
	return tryEarlyBdShimAt(args, stdin, stdout, stderr, time.Now())
}

func tryEarlyBdShimAt(args []string, stdin io.Reader, stdout, stderr io.Writer, mainStarted time.Time) (code int, handled bool) {
	code, handled, _ = tryEarlyBdShimOutcome(args, stdin, stdout, stderr, mainStarted)
	return code, handled
}

func tryEarlyBdShimOutcome(args []string, stdin io.Reader, stdout, stderr io.Writer, mainStarted time.Time) (code int, handled bool, legacy *earlyBdLegacyObservation) {
	if !bdFastpathEnabled(os.Getenv("GC_BD_FASTPATH")) {
		return 0, false, nil
	}
	if hasAmbientDoltOverride() {
		return 0, false, nil
	}
	cityPath := managedCityPath()
	if cityPath == "" {
		return 0, false, nil
	}
	bdArgs, ok := earlyBdShimShowArgs(args)
	if !ok {
		return 0, false, nil
	}
	shimPath, err := earlyBdShimPath(cityPath)
	if err != nil || shimPath == "" || !earlyBdShimIsOnPath(shimPath) {
		return 0, false, nil
	}
	return runEarlyBdExperiment(shimPath, bdArgs, stdin, stdout, stderr, mainStarted)
}

// tryEarlyBdShimRead routes controller-only reads directly to bdshim only when
// ordinary gc bd would execute that exact managed shim from PATH. That
// condition makes this a startup optimization of the already-selected
// controller behavior; a terminal whose normal gc bd resolves the real bd
// retains the full path and its rig-local/raw-bd contract.
func tryEarlyBdShimRead(args []string, stdin io.Reader, stdout, stderr io.Writer) (code int, handled bool) {
	return tryEarlyBdShimReadAt(args, stdin, stdout, stderr, time.Now())
}

func tryEarlyBdShimReadAt(args []string, stdin io.Reader, stdout, stderr io.Writer, mainStarted time.Time) (code int, handled bool) {
	code, handled, _ = tryEarlyBdShimReadOutcome(args, stdin, stdout, stderr, mainStarted)
	return code, handled
}

func tryEarlyBdShimReadOutcome(args []string, stdin io.Reader, stdout, stderr io.Writer, mainStarted time.Time) (code int, handled bool, legacy *earlyBdLegacyObservation) {
	if !bdFastpathEnabled(os.Getenv("GC_BD_FASTPATH")) {
		return 0, false, nil
	}
	if hasExplicitBdScopeFlag(args) || hasAmbientDoltOverride() {
		return 0, false, nil
	}
	cityPath := managedCityPath()
	if cityPath == "" {
		return 0, false, nil
	}
	bdArgs, ok := earlyBdShimReadArgs(args)
	if !ok {
		return 0, false, nil
	}
	shimPath, err := earlyBdShimPath(cityPath)
	if err != nil || shimPath == "" || !earlyBdShimIsOnPath(shimPath) {
		return 0, false, nil
	}
	return runEarlyBdExperiment(shimPath, bdArgs, stdin, stdout, stderr, mainStarted)
}

type earlyBdLegacyObservation struct {
	config bdexperiment.Config
	verb   string
	shape  bdexperiment.Shape
}

func runEarlyBdExperiment(shimPath string, bdArgs []string, stdin io.Reader, stdout, stderr io.Writer, mainStarted time.Time) (int, bool, *earlyBdLegacyObservation) {
	verb, verbArgs := bdshim.SplitGlobalFlags(bdArgs)
	shape, approved := earlyBdExperimentShape(verb, verbArgs)
	if !approved {
		return runEarlyBdShim(shimPath, bdArgs, stdin, stdout, stderr), true, nil
	}
	config := bdexperiment.Parse(os.Getenv)
	switch bdexperiment.SelectForShape(config, shape, earlyBdExperimentNext) {
	case bdexperiment.ArmLegacy:
		return 0, false, &earlyBdLegacyObservation{config: config, verb: verb, shape: shape}
	case bdexperiment.ArmDirect:
		target, ok := bdroute.Resolve("", os.Getenv, nil)
		if !ok {
			return 0, false, nil
		}
		return observeEarlyBdExperiment(config, bdexperiment.ArmDirect, verb, shape, stdout, mainStarted, func(observed io.Writer) int {
			return runEarlyBdDirect(target.BaseURL, target.City, verb, verbArgs, observed, stderr)
		}), true, nil
	default:
		return observeEarlyBdExperiment(config, bdexperiment.ArmShim, verb, shape, stdout, mainStarted, func(observed io.Writer) int {
			return runEarlyBdShim(shimPath, bdArgs, stdin, observed, stderr)
		}), true, nil
	}
}

func observeEarlyBdExperiment(config bdexperiment.Config, arm bdexperiment.Arm, verb string, shape bdexperiment.Shape, stdout io.Writer, mainStarted time.Time, run func(io.Writer) int) int {
	dispatcherStarted := time.Now()
	observed := &countingWriter{target: stdout}
	code := run(observed)
	generation := config.Generation
	if generation == "" {
		generation = "0"
	}
	_ = bdexperiment.Append(earlyBdExperimentLogPath(), bdexperiment.Record{
		Schema:           bdexperiment.SchemaVersion,
		Build:            version,
		Arm:              arm,
		Verb:             verb,
		Shape:            shape,
		Disposition:      "controller",
		Exit:             code,
		StdoutBytes:      observed.BytesWritten(),
		ConfigGeneration: generation,
		MainMS:           time.Since(mainStarted).Milliseconds(),
		DispatcherMS:     time.Since(dispatcherStarted).Milliseconds(),
	})
	return code
}

func observeLegacyBdExperiment(observation earlyBdLegacyObservation, stdoutBytes int64, mainStarted time.Time, exit int) {
	generation := observation.config.Generation
	if generation == "" {
		generation = "0"
	}
	_ = bdexperiment.Append(earlyBdExperimentLogPath(), bdexperiment.Record{
		Schema:           bdexperiment.SchemaVersion,
		Build:            version,
		Arm:              bdexperiment.ArmLegacy,
		Verb:             observation.verb,
		Shape:            observation.shape,
		Disposition:      "legacy",
		Exit:             exit,
		StdoutBytes:      stdoutBytes,
		ConfigGeneration: generation,
		MainMS:           time.Since(mainStarted).Milliseconds(),
		DispatcherMS:     0,
	})
}

func earlyBdExperimentLogPath() string {
	if path := strings.TrimSpace(os.Getenv("GC_BD_EXPERIMENT_LOG")); path != "" {
		return path
	}
	if city := managedCityPath(); city != "" {
		return filepath.Join(city, ".gc", "bd-experiment.jsonl")
	}
	return ""
}

func earlyBdExperimentShape(verb string, args []string) (bdexperiment.Shape, bool) {
	switch verb {
	case "show":
		if len(args) == 2 && args[1] == "--json" {
			return bdexperiment.ShapeShowJSON, true
		}
	case "list":
		if bdshim.ListRoutable(args) && !bdshim.ListHasMetadataPredicate(args) {
			return bdexperiment.ShapeListJSON, true
		}
	case "query":
		if bdshim.QueryRoutable(args) {
			return bdexperiment.ShapeQueryEphemeral, true
		}
	case "mol":
		sub, _, _, ok := bdshim.MolRoutable(args)
		if ok && sub == "current" {
			return bdexperiment.ShapeMolCurrent, true
		}
		if ok && sub == "progress" {
			return bdexperiment.ShapeMolProgress, true
		}
	}
	return "", false
}

// hasExplicitBdScopeFlag keeps explicit city and rig requests on doBd, which
// validates the requested scope before preparing a child command. bdshim can
// represent the route itself but does not own that gc-specific validation.
func hasExplicitBdScopeFlag(args []string) bool {
	for _, arg := range args[1:] {
		if arg == "--city" || arg == "--rig" || strings.HasPrefix(arg, "--city=") || strings.HasPrefix(arg, "--rig=") {
			return true
		}
	}
	return false
}

// hasAmbientDoltOverride keeps diagnostic and external-target resolution in
// doBd, which may warn when an inherited endpoint disagrees with the canonical
// configured target. The normal local managed path has neither variable set.
func hasAmbientDoltOverride() bool {
	return strings.TrimSpace(os.Getenv("GC_DOLT_HOST")) != "" || strings.TrimSpace(os.Getenv("GC_DOLT_PORT")) != ""
}

// earlyBdShimIsOnPath reports whether a normal gc bd child would resolve to
// the same trusted managed shim selected for the early path. Comparing resolved
// executables rather than text paths makes symlinked gc and city installs safe.
func earlyBdShimIsOnPath(shimPath string) bool {
	pathBD, err := earlyBdLookPath("bd")
	if err != nil {
		return false
	}
	shimResolved, ok := resolvedExecutablePath(shimPath)
	if !ok {
		return false
	}
	pathResolved, ok := resolvedExecutablePath(pathBD)
	return ok && pathResolved == shimResolved
}

// runEarlyBdShim preserves the managed shim's argv[0], standard streams, and
// non-zero exit contract for every early shim route.
func runEarlyBdShim(shimPath string, bdArgs []string, stdin io.Reader, stdout, stderr io.Writer) int {
	cmd := exec.Command(shimPath, bdArgs...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if exitCode := exitErr.ExitCode(); exitCode > 0 {
				return exitCode
			}
		}
		// The shim was selected successfully. Do not fall through to raw bd on
		// a launch error: a routed controller read must fail loudly rather than
		// risk reading the caller's unrelated cwd store.
		return 1
	}
	return 0
}

// runEarlyBdDirect serves an approved controller read in this gc process. It
// intentionally uses the same dispatcher as bdshim and has no raw-bd fallback.
func runEarlyBdDirect(baseURL, city, verb string, args []string, stdout, stderr io.Writer) int {
	return bddispatch.DispatchViaAPI(beadclient.NewCityScopedClient(baseURL, city), verb, args, stdout, stderr)
}

// earlyBdShimReadArgs accepts only read forms the shim's classifier serves
// through the controller. Shapes which might make the shim pass through to raw
// bd keep the normal gc bd path, because that path supplies its resolved scope
// and managed child environment. Writes stay on doBd even when the shim can
// route them, preserving gc's mutation guards and close gate.
func earlyBdShimReadArgs(args []string) ([]string, bool) {
	if len(args) < 2 || args[0] != "bd" {
		return nil, false
	}
	bdArgs := args[1:]
	_, _, unscoped := extractBdScopeFlags(bdArgs)
	verb, verbArgs := bdshim.SplitGlobalFlags(unscoped)
	switch verb {
	case "list", "ready", "query", "mol":
	default:
		return nil, false
	}
	if bdshim.ClassifyVerb(verb, verbArgs, false) != bdshim.Route {
		return nil, false
	}
	return bdArgs, true
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
