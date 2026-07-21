package main

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBdInvocationProfilerWritesProfilesAndRedactedPhaseReport(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GC_BD_PROFILE_DIR", dir)

	var stderr bytes.Buffer
	profiler := newBdInvocationProfiler([]string{"bd", "show", "gcw-secret", "--json"}, &stderr)
	end := profiler.phase("synthetic")
	end()
	profiler.close()

	if got := stderr.String(); got != "" {
		t.Fatalf("stderr = %q, want empty", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var reportPath string
	var gotCPU, gotRuntime bool
	for _, entry := range entries {
		switch {
		case strings.HasSuffix(entry.Name(), ".cpu.pprof"):
			gotCPU = true
		case strings.HasSuffix(entry.Name(), ".runtime.trace"):
			gotRuntime = true
		case strings.HasSuffix(entry.Name(), ".phases.json"):
			reportPath = filepath.Join(dir, entry.Name())
		}
	}
	if !gotCPU || !gotRuntime || reportPath == "" {
		t.Fatalf("profile artifacts: cpu=%t runtime=%t report=%q, entries=%v", gotCPU, gotRuntime, reportPath, entries)
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("profile artifact %q permissions = %o, want owner-only", entry.Name(), info.Mode().Perm())
		}
	}

	data, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	var report bdInvocationProfileReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("unmarshal report: %v\n%s", err, data)
	}
	if report.Command != "bd" {
		t.Fatalf("report command = %q, want bd", report.Command)
	}
	if strings.Contains(string(data), "gcw-secret") {
		t.Fatalf("phase report leaked invocation argument: %s", data)
	}
	if !report.hasPhase("synthetic") || !report.hasPhase("total") {
		t.Fatalf("report phases = %+v, want synthetic and total", report.Phases)
	}
	if diff := math.Abs(report.TotalMS - report.phaseDuration("total")); diff > 1 {
		t.Fatalf("report total_ms = %f, total phase = %f; difference %fms is diagnostic overhead, not command time", report.TotalMS, report.phaseDuration("total"), diff)
	}
}

func TestRunBdInvocationProfileCoversGCRoutingAndChild(t *testing.T) {
	silentFallbackTestSetup(t, "#!/bin/sh\nif [ -n \"${GC_BD_PROFILE_DIR:-}\" ]; then\n  echo profile-dir-leaked >&2\n  exit 97\nfi\nprintf 'profile-success\\n'\n")
	profileDir := t.TempDir()
	t.Setenv("GC_BD_PROFILE_DIR", profileDir)
	t.Setenv("GC_BD_FASTPATH", "0")

	var stdout, stderr bytes.Buffer
	if code := run([]string{"bd", "list", "demo-secret"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run(gc bd list) = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if got := stdout.String(); got != "profile-success\n" {
		t.Fatalf("stdout = %q, want profile-success", got)
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("stderr = %q, want empty", got)
	}

	entries, err := os.ReadDir(profileDir)
	if err != nil {
		t.Fatal(err)
	}
	var reportPath string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".phases.json") {
			reportPath = filepath.Join(profileDir, entry.Name())
		}
	}
	if reportPath == "" {
		t.Fatalf("missing phase report in %v", entries)
	}
	data, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	var report bdInvocationProfileReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("unmarshal report: %v\n%s", err, data)
	}
	for _, phase := range []string{
		"early_shim_probe", "telemetry_init", "command_tree", "command_execute",
		"rewrite_heartbeat", "resolve_city", "load_city_config", "resolve_scope",
		"config_builtin_pack_includes", "config_load_with_includes", "config_postprocess",
		"provider_preflight", "prepare_subprocess", "bd_subprocess", "total",
	} {
		if !report.hasPhase(phase) {
			t.Fatalf("profile phases = %+v, missing %q", report.Phases, phase)
		}
	}
	if strings.Contains(string(data), "demo-secret") {
		t.Fatalf("phase report leaked invocation argument: %s", data)
	}
	if diff := math.Abs(report.TotalMS - report.phaseDuration("total")); diff > 1 {
		t.Fatalf("report total_ms = %f, total phase = %f; difference %fms is diagnostic overhead, not command time", report.TotalMS, report.phaseDuration("total"), diff)
	}
}

func TestBdInvocationProfilerIsDisabledForNonBdInvocation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GC_BD_PROFILE_DIR", dir)

	profiler := newBdInvocationProfiler([]string{"status"}, &bytes.Buffer{})
	profiler.phase("synthetic")()
	profiler.close()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("non-bd invocation wrote profile artifacts: %v", entries)
	}
}
