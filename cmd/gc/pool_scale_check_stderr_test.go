package main

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

func TestShellCommandWithStderr_CapturesBoth(t *testing.T) {
	cmd := `printf "count=5\n"; printf "veto: memory high\n" >&2`
	stdout, stderr, err := shellCommandWithStderr(cmd, "", 5*time.Second, nil)
	if err != nil {
		t.Fatalf("shellCommandWithStderr failed: %v", err)
	}
	if strings.TrimSpace(stdout) != "count=5" {
		t.Errorf("stdout = %q, want count=5", stdout)
	}
	if strings.TrimSpace(stderr) != "veto: memory high" {
		t.Errorf("stderr = %q, want 'veto: memory high'", stderr)
	}
}

func TestEvaluatePoolWithStderr_PreservesStderr(t *testing.T) {
	sp := scaleParams{
		Min:   0,
		Max:   10,
		Check: "fake-check",
	}
	mockRunner := func(_, _ string, _ map[string]string) (string, string, error) {
		return "0\n", "veto: load5=15.0_exceeds_12.0\n", nil
	}

	desired, stderrOut, err := evaluatePoolWithStderr("worker", sp, "", nil, mockRunner)
	if err != nil {
		t.Fatalf("evaluatePoolWithStderr error: %v", err)
	}
	if desired != 0 {
		t.Errorf("desired = %d, want 0", desired)
	}
	if !strings.Contains(stderrOut, "veto: load5=15.0_exceeds_12.0") {
		t.Errorf("stderrOut = %q, want to contain veto reason", stderrOut)
	}
}

func TestEvaluatePoolNewDemandWithStderr_PreservesStderr(t *testing.T) {
	sp := scaleParams{
		Min:   0,
		Max:   10,
		Check: "fake-check",
	}
	mockRunner := func(_, _ string, _ map[string]string) (string, string, error) {
		return "0\n", "veto: memory_psi_avg10=1.50_exceeds_1.00\n", nil
	}

	desired, stderrOut, err := evaluatePoolNewDemandWithStderr("worker", sp, "", nil, mockRunner)
	if err != nil {
		t.Fatalf("evaluatePoolNewDemandWithStderr error: %v", err)
	}
	if desired != 0 {
		t.Errorf("desired = %d, want 0", desired)
	}
	if !strings.Contains(stderrOut, "veto: memory_psi_avg10=1.50_exceeds_1.00") {
		t.Errorf("stderrOut = %q, want to contain veto reason", stderrOut)
	}
}

func TestScaleCheckTrace_AttachesStderr(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{
				Name: "worker",
			},
		},
	}

	trace := &sessionReconcilerTraceCycle{
		tracer: &SessionReconcilerTracer{
			detail: map[string]TraceSource{"worker": TraceSourceManual},
		},
		dropReasons:       map[string]int{},
		pendingDetail:     map[string][]SessionReconcilerTraceRecord{},
		pendingDropped:    map[string]int{},
		templatesTouched:  map[string]struct{}{},
		detailedTemplates: map[string]struct{}{},
		decisionCounts:    map[string]int{},
		operationCounts:   map[string]int{},
		mutationCounts:    map[string]int{},
		reasonCounts:      map[string]int{},
		outcomeCounts:     map[string]int{},
	}

	pending := []poolEvalWork{
		{
			agentIdx: 0,
			sp: scaleParams{
				Min:   0,
				Max:   5,
				Check: `printf "0\n"; printf "veto: memory_psi_avg10=1.50_exceeds_1.00\n" >&2`,
			},
		},
	}

	counts, partials := evaluatePendingPools(cfg, pending, io.Discard, trace)
	if len(counts) != 1 || counts[0] != 0 {
		t.Fatalf("counts = %v, want [0]", counts)
	}
	if len(partials) != 1 || partials[0] {
		t.Fatalf("partials = %v, want [false]", partials)
	}

	// Verify that trace contains TraceSiteScaleCheckExec with stderr
	var foundScaleCheck bool
	for _, op := range trace.records {
		if op.SiteCode == TraceSiteScaleCheckExec {
			foundScaleCheck = true
			stderrVal, ok := op.Fields["stderr"].(string)
			if !ok {
				t.Fatalf("expected op.Fields['stderr'] to be string, got %T: %v", op.Fields["stderr"], op.Fields)
			}
			if !strings.Contains(stderrVal, "memory_psi_avg10=1.50_exceeds_1.00") {
				t.Errorf("trace payload stderr = %q, want containing memory_psi_avg10", stderrVal)
			}
			break
		}
	}
	if !foundScaleCheck {
		t.Error("TraceSiteScaleCheckExec operation not found in trace.records")
	}
}
