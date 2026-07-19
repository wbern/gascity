package config

import (
	"fmt"
	"strings"
)

// bd-shim routing modes for [session] bd_shim. The bd-shim (bdproxy) is a tiny
// thin client installed beside gc that routes a worker's hot `bd` calls to the
// warm controller instead of paying gc's cold-start per call.
const (
	// BdShimModeAuto routes through the bd-shim thin client when it is installed
	// beside gc, else uses the real bd. The default and upstream-safe no-op.
	BdShimModeAuto = "auto"
	// BdShimModeOn always installs the redirect; doctor warns if the thin client
	// binary is missing.
	BdShimModeOn = "on"
	// BdShimModeOff never installs the redirect; workers use the real bd directly.
	BdShimModeOff = "off"
)

// BdShimEnvOverride is the environment variable that overrides [session]
// bd_shim at the shim-install site (for ephemeral benchmarking without editing
// config), mirroring GC_READY_FEDERATION_CONCURRENCY. Precedence: this env >
// [session] bd_shim > BdShimModeAuto.
const BdShimEnvOverride = "GC_BDSHIM"

// BdShimMode returns the configured bd-shim routing mode, one of
// BdShimModeAuto, BdShimModeOn, or BdShimModeOff. An empty or unrecognized
// value resolves to BdShimModeAuto; load-time validation (ValidateBdShim)
// rejects unrecognized values, so the defensive fallback only guards
// programmatic callers. The GC_BDSHIM env override is applied at the
// shim-install site (see cmd/gc), not here, to keep the config layer pure.
func (s *SessionConfig) BdShimMode() string {
	switch strings.TrimSpace(s.BdShim) {
	case BdShimModeOn:
		return BdShimModeOn
	case BdShimModeOff:
		return BdShimModeOff
	default:
		return BdShimModeAuto
	}
}

// NormalizeBdShimMode maps a raw mode string (e.g. a GC_BDSHIM env value) to a
// recognized mode, defaulting to BdShimModeAuto for empty or unrecognized
// input. Accepts the numeric aliases "1" (on) and "0" (off) for convenience in
// shell benchmarking.
func NormalizeBdShimMode(raw string) string {
	switch strings.TrimSpace(raw) {
	case BdShimModeOn, "1":
		return BdShimModeOn
	case BdShimModeOff, "0":
		return BdShimModeOff
	default:
		return BdShimModeAuto
	}
}

// ValidateBdShim rejects an unrecognized [session] bd_shim value at config
// load, mirroring the default_sling_strategy enum guard. Empty is valid (it
// resolves to the auto default).
func ValidateBdShim(cfg *City) error {
	switch strings.TrimSpace(cfg.Session.BdShim) {
	case "", BdShimModeAuto, BdShimModeOn, BdShimModeOff:
		return nil
	default:
		return fmt.Errorf("invalid [session] bd_shim %q (want %q, %q, or %q)",
			cfg.Session.BdShim, BdShimModeAuto, BdShimModeOn, BdShimModeOff)
	}
}
