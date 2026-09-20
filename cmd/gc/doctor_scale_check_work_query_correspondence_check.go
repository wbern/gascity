package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// scaleCheckWorkQueryCorrespondenceRunner executes a command via sh -c with
// working directory and environment, returning stdout, stderr, and execution error.
type scaleCheckWorkQueryCorrespondenceRunner func(command, dir string, env map[string]string) (stdout, stderr string, err error)

// scaleCheckWorkQueryCorrespondenceCheck verifies that agents with BOTH a
// custom scale_check and a custom work_query observe the bead store through
// symmetric filters. It runs both predicates and warns when demand calculation
// and claim logic diverge (e.g. reconciler spawns sessions for work claim refuses).
type scaleCheckWorkQueryCorrespondenceCheck struct {
	cfg      *config.City
	cityPath string
	runner   scaleCheckWorkQueryCorrespondenceRunner
}

func newScaleCheckWorkQueryCorrespondenceCheck(cfg *config.City, cityPath string) *scaleCheckWorkQueryCorrespondenceCheck {
	return newScaleCheckWorkQueryCorrespondenceCheckWithRunner(cfg, cityPath, func(command, dir string, env map[string]string) (string, string, error) {
		return shellCommandWithStderr(command, dir, 10*time.Second, env)
	})
}

func newScaleCheckWorkQueryCorrespondenceCheckWithRunner(cfg *config.City, cityPath string, runner scaleCheckWorkQueryCorrespondenceRunner) *scaleCheckWorkQueryCorrespondenceCheck {
	return &scaleCheckWorkQueryCorrespondenceCheck{
		cfg:      cfg,
		cityPath: cityPath,
		runner:   runner,
	}
}

func (c *scaleCheckWorkQueryCorrespondenceCheck) Name() string {
	return "scale-check-work-query-correspondence"
}

func (c *scaleCheckWorkQueryCorrespondenceCheck) CanFix() bool { return false }

func (c *scaleCheckWorkQueryCorrespondenceCheck) Fix(_ *doctor.CheckContext) error { return nil }

func (c *scaleCheckWorkQueryCorrespondenceCheck) WarmupEligible() bool { return false }

func (c *scaleCheckWorkQueryCorrespondenceCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	if c.cfg == nil {
		return okCheck(c.Name(), "no config available")
	}

	var findings []string
	checked := 0

	for _, a := range c.cfg.Agents {
		// Only check agents that have BOTH custom scale_check and custom work_query.
		// Built-in predicates are structurally bound and guarded by config tests.
		// Single-side overrides are caught by config-semantics validation.
		if a.ScaleCheck == "" || a.WorkQuery == "" {
			continue
		}
		checked++

		dir := c.cityPath
		if rigName := configuredRigName(c.cityPath, &a, c.cfg.Rigs); rigName != "" {
			if rigRoot := rigRootForName(rigName, c.cfg.Rigs); rigRoot != "" {
				dir = rigRoot
			}
		}

		env, _ := controllerWorkQueryEnv(c.cityPath, c.cfg, &a)

		scaleCmd := expandAgentCommandTemplate(c.cityPath, c.cfg.Workspace.Name, &a, c.cfg.Rigs, "scale_check", a.ScaleCheck, nil)
		workCmd := expandAgentCommandTemplate(c.cityPath, c.cfg.Workspace.Name, &a, c.cfg.Rigs, "work_query", a.WorkQuery, nil)

		scaleOut, scaleStderr, scaleErr := c.runner(scaleCmd, dir, env)
		if scaleErr != nil {
			findings = append(findings, fmt.Sprintf("agent %q: custom scale_check failed: %v (%s)",
				a.QualifiedName(), scaleErr, strings.TrimSpace(scaleStderr)))
			continue
		}

		demand, err := strconv.Atoi(strings.TrimSpace(scaleOut))
		if err != nil {
			findings = append(findings, fmt.Sprintf("agent %q: custom scale_check produced non-integer output %q",
				a.QualifiedName(), strings.TrimSpace(scaleOut)))
			continue
		}

		workOut, workStderr, workErr := c.runner(workCmd, dir, env)
		if workErr != nil {
			if demand > 0 {
				findings = append(findings, fmt.Sprintf("agent %q: custom scale_check reports %d demand, but custom work_query failed or refused: %v (%s)",
					a.QualifiedName(), demand, workErr, strings.TrimSpace(workStderr)))
			}
			continue
		}

		claimCount := parseWorkQueryCandidateCount(workOut)

		if demand > 0 && claimCount == 0 {
			findings = append(findings, fmt.Sprintf("agent %q: custom scale_check reports %d demand, but custom work_query returns 0 claimable beads (reconciler may spawn sessions that immediately exit)",
				a.QualifiedName(), demand))
		} else if demand == 0 && claimCount > 0 {
			findings = append(findings, fmt.Sprintf("agent %q: custom work_query returns %d claimable bead(s), but custom scale_check reports 0 demand (reconciler will not spawn sessions for available work)",
				a.QualifiedName(), claimCount))
		}
	}

	if len(findings) > 0 {
		sort.Strings(findings)
		msg := fmt.Sprintf("%d agent(s) have scale_check and work_query correspondence divergence", len(findings))
		return warnCheck(c.Name(), msg, "align custom scale_check and work_query predicates so demand and claim logic observe the same work units", findings)
	}

	if checked > 0 {
		return okCheck(c.Name(), fmt.Sprintf("%d agent(s) with custom scale_check and work_query agree", checked))
	}
	return okCheck(c.Name(), "no agents with dual custom scale_check and work_query configured")
}

func parseWorkQueryCandidateCount(output string) int {
	trimmed := strings.TrimSpace(output)
	if trimmed == "" || trimmed == "[]" {
		return 0
	}
	var arr []any
	if err := json.Unmarshal([]byte(trimmed), &arr); err == nil {
		return len(arr)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(trimmed), &obj); err == nil && len(obj) > 0 {
		return 1
	}
	return 0
}
