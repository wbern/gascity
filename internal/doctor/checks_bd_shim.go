package doctor

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/config"
)

// bdshimBinName is the file name of the bd thin client installed beside the gc
// binary. It mirrors cmd/gc's bdshimBinName; the doctor package cannot import
// cmd/gc, so the constant and the beside-exe resolver are duplicated here with
// stdlib only.
const bdshimBinName = "bdshim"

// BdShimCheck reports the effective [session] bd_shim routing mode and whether
// the bdshim thin client is installed beside gc. It is pure observability plus a
// single actionable warning: bd_shim=on with no bdshim binary present means a
// worker's `bd` silently falls back to the real bd, losing the intended
// cold-start savings. See gcw-b8yk.
type BdShimCheck struct {
	cfg   *config.City
	gcExe string
}

// NewBdShimCheck creates a bd-shim routing check against the running gc binary's
// directory. cfg may be nil (a broken city.toml), in which case the mode
// defaults to auto.
func NewBdShimCheck(cfg *config.City) *BdShimCheck {
	exe, _ := os.Executable()
	return &BdShimCheck{cfg: cfg, gcExe: exe}
}

// Name returns the check identifier.
func (c *BdShimCheck) Name() string { return "bd-shim" }

// Run reports the resolved bd_shim mode and bdshim binary presence, warning only
// when bd_shim=on but the thin client is missing.
func (c *BdShimCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}

	mode := config.BdShimModeAuto
	if c.cfg != nil {
		mode = c.cfg.Session.BdShimMode()
	}
	present := bdshimBesidePath(c.gcExe)

	switch {
	case mode == config.BdShimModeOff:
		r.Status = StatusOK
		r.Message = "bd-shim routing disabled (bd_shim=off); workers use real bd"
	case mode == config.BdShimModeOn && !present:
		r.Status = StatusWarning
		r.Message = "bd_shim=on but no bdshim beside gc; bd falls back to real bd"
		r.FixHint = "build and install the bdshim binary beside gc (make bdshim), or set [session] bd_shim=auto"
	case present:
		r.Status = StatusOK
		r.Message = fmt.Sprintf("bd-shim routing active (mode=%s, bdshim present)", mode)
	default: // auto with no bdshim present — upstream-safe no-op
		r.Status = StatusOK
		r.Message = "bd_shim=auto, no bdshim present; workers use real bd (no-op)"
	}
	return r
}

// CanFix returns false. The check is observability plus a build/config hint; the
// remediation (build the binary or change config) is an operator action.
func (c *BdShimCheck) CanFix() bool { return false }

// Fix is a no-op. See CanFix.
func (c *BdShimCheck) Fix(_ *CheckContext) error { return nil }

// WarmupEligible returns false: bd-shim routing is steady-state observability,
// not a startup gate, so it runs on demand via `gc doctor` only.
func (c *BdShimCheck) WarmupEligible() bool { return false }

// bdshimBesidePath reports whether an executable bdshim binary exists in the same
// directory as exe, or as exe's symlink-resolved target. It mirrors cmd/gc's
// bdshimBesideExe using stdlib only, so a gc started via a symlink (e.g.
// .local/bin/gc -> go/bin/gc) still finds a bdshim installed beside either.
func bdshimBesidePath(exe string) bool {
	if exe == "" {
		return false
	}
	candidates := []string{filepath.Join(filepath.Dir(exe), bdshimBinName)}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil && resolved != exe {
		candidates = append(candidates, filepath.Join(filepath.Dir(resolved), bdshimBinName))
	}
	for _, cand := range candidates {
		if info, err := os.Stat(cand); err == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0 {
			return true
		}
	}
	return false
}
