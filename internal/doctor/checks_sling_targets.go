package doctor

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

// SlingTargetsOverrideCheck warns when a rig sets both default_sling_target
// and default_sling_targets. Targetless `gc sling` prefers the plural form, so
// the singular default_sling_target becomes dead config — usually a leftover
// from migrating a rig to multi-target dispatch, and a likely source of
// confusion about where work actually routes.
type SlingTargetsOverrideCheck struct {
	rigs []config.Rig
}

// NewSlingTargetsOverrideCheck builds the check from city config.
func NewSlingTargetsOverrideCheck(cfg *config.City) *SlingTargetsOverrideCheck {
	var rigs []config.Rig
	if cfg != nil {
		rigs = cfg.Rigs
	}
	return &SlingTargetsOverrideCheck{rigs: rigs}
}

// Name identifies the check.
func (c *SlingTargetsOverrideCheck) Name() string { return "sling-default-targets" }

// CanFix reports that this check has no automatic remediation.
func (c *SlingTargetsOverrideCheck) CanFix() bool { return false }

// Fix is a no-op; the resolution is an operator config edit.
func (c *SlingTargetsOverrideCheck) Fix(*CheckContext) error { return nil }

// WarmupEligible keeps this advisory check out of the start-up warm-up scan.
func (c *SlingTargetsOverrideCheck) WarmupEligible() bool { return false }

// Run flags rigs that set both default_sling_target and default_sling_targets.
func (c *SlingTargetsOverrideCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name(), Severity: SeverityAdvisory}
	var shadowed []string
	for _, rig := range c.rigs {
		if len(rig.DefaultSlingTargets) > 0 && strings.TrimSpace(rig.DefaultSlingTarget) != "" {
			shadowed = append(shadowed, rig.Name)
		}
	}
	if len(shadowed) == 0 {
		r.Status = StatusOK
		r.Message = "no rig sets both default_sling_target and default_sling_targets"
		return r
	}
	r.Status = StatusWarning
	r.Message = fmt.Sprintf(
		"rig(s) %s set both default_sling_target and default_sling_targets; default_sling_targets wins and default_sling_target is ignored",
		strings.Join(shadowed, ", "),
	)
	r.FixHint = "remove default_sling_target from these rigs, or fold its value into default_sling_targets"
	for _, n := range shadowed {
		r.Details = append(r.Details, fmt.Sprintf("rig %q: default_sling_target is dead config (default_sling_targets takes precedence)", n))
	}
	return r
}
