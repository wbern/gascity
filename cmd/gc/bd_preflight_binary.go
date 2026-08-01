package main

import (
	"os"
	"path/filepath"
	"strings"
)

// realBdEnvVar names the environment variable holding the absolute path of the
// real bd binary. cmd/bdshim reads the same variable to resolve its own
// passthrough target, so a value honored here reaches exactly the binary a
// passthrough would have ended at.
const realBdEnvVar = "GC_BD_REAL"

// bdShimBinaryName is the shim's installed binary name. A GC_BD_REAL naming it
// is ignored: the shim resolves bd through this same variable, so spawning it
// would put the hop back and hand the shim a self-referential target.
const bdShimBinaryName = "bdshim"

// preflightBDBinary returns the bd binary gc's own preflight should spawn.
//
// The preflight's `bd context --json` is gc talking to itself — it is the only
// caller of that verb in the tree — and spawning the literal "bd" resolved
// through PATH to the shim installed ahead of it. The shim routes no context
// call (it is 100% passthrough), so each one paid a process hop only to reach
// the binary gc could have spawned directly. Measured: 21,780 of 21,782 logged
// context calls came from this one call site, 24.7% of all shim traffic.
//
// It falls back to the PATH lookup whenever GC_BD_REAL is unset or unusable.
// The variable is set for sessions and order children but not provably for
// every context that runs preflight, and a store open must not start failing
// because an environment lacks it.
func preflightBDBinary() string {
	raw := strings.TrimSpace(os.Getenv(realBdEnvVar))
	if raw == "" || !filepath.IsAbs(raw) {
		return "bd"
	}
	if filepath.Base(raw) == bdShimBinaryName {
		return "bd"
	}
	info, err := os.Stat(raw)
	if err != nil || info.IsDir() {
		return "bd"
	}
	return raw
}
