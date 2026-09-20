package doctor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// AdmissionStateCheck reads any *-admission.json under .gc/runtime/ and warns
// when blocked is true, naming the reason.
type AdmissionStateCheck struct {
	cityPath string
}

// NewAdmissionStateCheck creates a new admission state doctor check.
func NewAdmissionStateCheck(cityPath string) *AdmissionStateCheck {
	return &AdmissionStateCheck{cityPath: cityPath}
}

// Name returns the doctor check identifier.
func (c *AdmissionStateCheck) Name() string { return "admission-state" }

// WarmupEligible returns false; this check is not part of the `gc start` warm-up scan.
func (c *AdmissionStateCheck) WarmupEligible() bool { return false }

// CanFix returns false: admission policies require human or workload remediation.
func (c *AdmissionStateCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *AdmissionStateCheck) Fix(_ *CheckContext) error { return nil }

type rawAdmissionFile struct {
	Blocked          *bool                         `json:"blocked"`
	LastReason       string                        `json:"last_reason"`
	Reason           string                        `json:"reason"`
	RecoveryPolicies map[string]rawAdmissionPolicy `json:"recovery_policies"`
}

type rawAdmissionPolicy struct {
	Blocked    *bool  `json:"blocked"`
	LastReason string `json:"last_reason"`
	Reason     string `json:"reason"`
}

// Run scans .gc/runtime/*-admission.json files and warns if any are blocked.
func (c *AdmissionStateCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}

	runtimeDir := filepath.Join(c.cityPath, ".gc", "runtime")
	matches, err := filepath.Glob(filepath.Join(runtimeDir, "*-admission.json"))
	if err != nil || len(matches) == 0 {
		r.Status = StatusOK
		r.Message = "no blocked admission gates"
		return r
	}

	sort.Strings(matches)
	var warnings []string

	for _, path := range matches {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var file rawAdmissionFile
		if err := json.Unmarshal(data, &file); err != nil {
			continue
		}

		base := filepath.Base(path)
		gateName := strings.TrimSuffix(base, "-admission.json")

		// 1. Top-level blocked
		if file.Blocked != nil && *file.Blocked {
			reason := file.LastReason
			if reason == "" {
				reason = file.Reason
			}
			if reason == "" {
				reason = "blocked"
			}
			warnings = append(warnings, fmt.Sprintf("%s: %s", gateName, reason))
		}

		// 2. Recovery policies blocked
		if len(file.RecoveryPolicies) > 0 {
			var polNames []string
			for pName := range file.RecoveryPolicies {
				polNames = append(polNames, pName)
			}
			sort.Strings(polNames)
			for _, pName := range polNames {
				pol := file.RecoveryPolicies[pName]
				if pol.Blocked != nil && *pol.Blocked {
					reason := pol.LastReason
					if reason == "" {
						reason = pol.Reason
					}
					if reason == "" {
						reason = "blocked"
					}
					warnings = append(warnings, fmt.Sprintf("%s/%s: %s", gateName, pName, reason))
				}
			}
		}
	}

	if len(warnings) == 0 {
		r.Status = StatusOK
		r.Message = "no blocked admission gates"
		return r
	}

	r.Status = StatusWarning
	r.Message = fmt.Sprintf("admission veto active: %s", strings.Join(warnings, "; "))
	r.FixHint = "check system resources or admission policy configuration"
	return r
}
