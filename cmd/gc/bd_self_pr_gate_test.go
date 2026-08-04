package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bdshim"
	"github.com/gastownhall/gascity/internal/beads"
)

func TestRejectSelfPRGate(t *testing.T) {
	t.Parallel()

	target := beads.Bead{
		ID:       "crm-1",
		Metadata: beads.StringMap{"pr_number": "987"},
	}
	if err := rejectSelfPRGate(target, bdshim.PRGateCreate{TargetID: "crm-1", PRNumber: "987"}); err == nil {
		t.Fatal("self-PR gate accepted")
	} else if !strings.Contains(err.Error(), "would deadlock its own repair") {
		t.Fatalf("rejection did not explain invariant: %v", err)
	}

	if err := rejectSelfPRGate(target, bdshim.PRGateCreate{TargetID: "crm-1", PRNumber: "988"}); err != nil {
		t.Fatalf("different PR rejected: %v", err)
	}
	if err := rejectSelfPRGate(beads.Bead{ID: "crm-1"}, bdshim.PRGateCreate{TargetID: "crm-1", PRNumber: "987"}); err != nil {
		t.Fatalf("bead without owned PR rejected: %v", err)
	}
}

func TestRunBdSelfPRGateGuardFailsClosedOnUnreadableTarget(t *testing.T) {
	t.Parallel()

	want := errors.New("store unavailable")
	err := runBdSelfPRGateGuard(
		[]string{"gate", "create", "--blocks", "crm-1", "--type", "gh:pr", "--await-id", "987"},
		func(string) (beads.Bead, error) { return beads.Bead{}, want },
	)
	if !errors.Is(err, want) {
		t.Fatalf("guard error = %v, want wrapped %v", err, want)
	}
}

func TestDoBdRunsSelfPRGuardBeforeBdSubprocess(t *testing.T) {
	silentFallbackTestSetup(t, silentFallbackFakeBdScript)

	original := bdSelfPRGateGuard
	t.Cleanup(func() { bdSelfPRGateGuard = original })
	called := false
	bdSelfPRGateGuard = func(_ []string, _ func(string) (beads.Bead, error)) error {
		called = true
		return errors.New("sentinel self-PR refusal")
	}

	var stdout, stderr bytes.Buffer
	code := doBd(
		[]string{"gate", "create", "--blocks", "demo-abc", "--type", "gh:pr", "--await-id", "987"},
		&stdout,
		&stderr,
	)
	if code == 0 {
		t.Fatal("guard refusal was ignored")
	}
	if !called {
		t.Fatal("gc bd did not invoke the self-PR guard")
	}
	if !strings.Contains(stderr.String(), "sentinel self-PR refusal") {
		t.Fatalf("stderr = %q, want guard refusal", stderr.String())
	}
	if strings.Contains(stderr.String(), "auto-importing") {
		t.Fatalf("bd subprocess ran after guard refusal: %q", stderr.String())
	}
}
