package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

func TestScaleCheckWorkQueryCorrespondenceCheck_WarnsWhenDemandCountsRowsClaimRefuses(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{{
			Name:       "reviewer",
			ScaleCheck: "printf 1",
			WorkQuery:  "reviewer-work-query.sh",
		}},
	}

	check := newScaleCheckWorkQueryCorrespondenceCheckWithRunner(cfg, cityDir, func(cmd, _ string, _ map[string]string) (string, string, error) {
		if strings.Contains(cmd, "printf 1") {
			return "1\n", "", nil
		}
		if strings.Contains(cmd, "reviewer-work-query.sh") {
			// Simulates claim side returning empty list (claim refused/unmatched)
			return "[]\n", "", nil
		}
		return "", "", fmt.Errorf("unexpected command %q", cmd)
	})

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning: %#v", result.Status, result)
	}
	details := strings.Join(result.Details, "\n")
	if !strings.Contains(details, "reviewer") {
		t.Errorf("details should mention agent name, got: %s", details)
	}
	if !strings.Contains(details, "scale_check reports 1 demand") {
		t.Errorf("details should mention scale_check demand, got: %s", details)
	}
	if !strings.Contains(details, "work_query returns 0 claimable") {
		t.Errorf("details should mention work_query 0 claimable beads, got: %s", details)
	}
}

func TestScaleCheckWorkQueryCorrespondenceCheck_WarnsWhenWorkQueryFails(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{{
			Name:       "reviewer",
			ScaleCheck: "printf 2",
			WorkQuery:  "reviewer-work-query.sh",
		}},
	}

	check := newScaleCheckWorkQueryCorrespondenceCheckWithRunner(cfg, cityDir, func(cmd, _ string, _ map[string]string) (string, string, error) {
		if strings.Contains(cmd, "printf 2") {
			return "2\n", "", nil
		}
		if strings.Contains(cmd, "reviewer-work-query.sh") {
			// Simulates GC3 exit 3 custody mismatch brake
			return "", "custody mismatch\n", errors.New("exit status 3")
		}
		return "", "", fmt.Errorf("unexpected command %q", cmd)
	})

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning: %#v", result.Status, result)
	}
	details := strings.Join(result.Details, "\n")
	if !strings.Contains(details, "reviewer") || !strings.Contains(details, "failed") {
		t.Errorf("details should mention agent and failure, got: %s", details)
	}
}

func TestScaleCheckWorkQueryCorrespondenceCheck_WarnsWhenClaimSeesWorkDemandMisses(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{{
			Name:       "worker",
			ScaleCheck: "printf 0",
			WorkQuery:  "worker-work-query.sh",
		}},
	}

	check := newScaleCheckWorkQueryCorrespondenceCheckWithRunner(cfg, cityDir, func(cmd, _ string, _ map[string]string) (string, string, error) {
		if strings.Contains(cmd, "printf 0") {
			return "0\n", "", nil
		}
		if strings.Contains(cmd, "worker-work-query.sh") {
			return `[{"id":"task-123"}]`, "", nil
		}
		return "", "", fmt.Errorf("unexpected command %q", cmd)
	})

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning: %#v", result.Status, result)
	}
	details := strings.Join(result.Details, "\n")
	if !strings.Contains(details, "worker") || !strings.Contains(details, "scale_check reports 0 demand") {
		t.Errorf("details should mention worker and 0 demand, got: %s", details)
	}
}

func TestScaleCheckWorkQueryCorrespondenceCheck_SilentWhenBothAgreeZero(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{{
			Name:       "reviewer",
			ScaleCheck: "printf 0",
			WorkQuery:  "reviewer-work-query.sh",
		}},
	}

	check := newScaleCheckWorkQueryCorrespondenceCheckWithRunner(cfg, cityDir, func(cmd, _ string, _ map[string]string) (string, string, error) {
		if strings.Contains(cmd, "printf 0") {
			return "0\n", "", nil
		}
		if strings.Contains(cmd, "reviewer-work-query.sh") {
			return "[]\n", "", nil
		}
		return "", "", fmt.Errorf("unexpected command %q", cmd)
	})

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want OK: %#v", result.Status, result)
	}
}

func TestScaleCheckWorkQueryCorrespondenceCheck_SilentWhenBothAgreePositive(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{{
			Name:       "reviewer",
			ScaleCheck: "printf 1",
			WorkQuery:  "reviewer-work-query.sh",
		}},
	}

	check := newScaleCheckWorkQueryCorrespondenceCheckWithRunner(cfg, cityDir, func(cmd, _ string, _ map[string]string) (string, string, error) {
		if strings.Contains(cmd, "printf 1") {
			return "1\n", "", nil
		}
		if strings.Contains(cmd, "reviewer-work-query.sh") {
			return `[{"id":"task-123"}]`, "", nil
		}
		return "", "", fmt.Errorf("unexpected command %q", cmd)
	})

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want OK: %#v", result.Status, result)
	}
}

func TestScaleCheckWorkQueryCorrespondenceCheck_SkipsDefaultPredicates(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{{
			Name: "default-agent",
			// Both ScaleCheck and WorkQuery empty -> defaults
		}},
	}

	runnerCalled := false
	check := newScaleCheckWorkQueryCorrespondenceCheckWithRunner(cfg, cityDir, func(_, _ string, _ map[string]string) (string, string, error) {
		runnerCalled = true
		return "", "", errors.New("should not be called")
	})

	result := check.Run(&doctor.CheckContext{})
	if runnerCalled {
		t.Fatal("runner should not have been called for agent with default predicates")
	}
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want OK: %#v", result.Status, result)
	}
}

func TestBuildDoctorChecksRegistersScaleCheckWorkQueryCorrespondenceCheck(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_DOLT", "skip")
	cfg := &config.City{Workspace: config.Workspace{Name: "demo"}}

	names := doctorCheckNames(buildDoctorChecks(cityDir, cfg, nil, buildDoctorChecksOpts{
		ControllerRunning:    false,
		SkipCityDoltCheck:    true,
		SkipManagedDoltCheck: true,
	}))

	for _, name := range names {
		if name == "scale-check-work-query-correspondence" {
			return
		}
	}
	t.Fatalf("scale-check-work-query-correspondence check not registered in buildDoctorChecks: %v", names)
}
