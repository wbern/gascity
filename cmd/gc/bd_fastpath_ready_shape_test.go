package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/bdexperiment"
	"github.com/gastownhall/gascity/internal/bdshim"
)

// `bd ready` is ~8.3% of measured fleet bd traffic and ClassifyVerb has always
// returned Route for it — but it was absent from earlyBdExperimentShape's
// allowlist, so gc could only serve it by exec'ing the bdshim BINARY. That made
// retiring the shim a regression for this verb: with no shim on disk the shape
// declines to doBd, which measured ~420 ms of client CPU against the shim
// path's ~22.6 ms.
//
// Serving it in-process is byte-safe by construction rather than by luck:
// cmd/bdshim/main.go and runEarlyBdDirect both build
// beadclient.NewCityScopedClient over a bdroute-resolved target and hand it to
// the same bddispatch.DispatchViaAPI. Same resolver, same client, same
// dispatcher, same args.

func TestEarlyBdExperimentShapeApprovesRoutableReady(t *testing.T) {
	// Every flag in bdshim.ReadyRoutableFlags, including the discovery
	// predicates the controller serve loop and pool-demand probe depend on.
	cases := [][]string{
		nil,
		{"--json"},
		{"--json", "--limit", "20"},
		{"--json", "-n", "5"},
		{"--assignee", "someone"},
		{"--json", "--unassigned"},
		{"--json", "--exclude-type", "chore"},
		{"--json", "--metadata-field", "gc.routed_to"},
		{"--json", "--include-ephemeral"},
		{"--json", "--sort", "created"},
	}
	for _, args := range cases {
		if !bdshim.ReadyRoutable(args) {
			t.Fatalf("precondition: ReadyRoutable(%v) = false, test case is wrong", args)
		}
		shape, ok := earlyBdExperimentShape("ready", args)
		if !ok {
			t.Errorf("earlyBdExperimentShape(ready, %v) not approved, want approved", args)
			continue
		}
		if shape != bdexperiment.ShapeReadyJSON {
			t.Errorf("earlyBdExperimentShape(ready, %v) = %q, want %q",
				args, shape, bdexperiment.ShapeReadyJSON)
		}
	}
}

// A ready form the shim itself would not route must not be approved here.
// ClassifyVerb already gates this upstream, but the shape function is the
// second gate and must not widen the contract on its own.
func TestEarlyBdExperimentShapeDeclinesUnroutableReady(t *testing.T) {
	for _, args := range [][]string{
		{"--not-a-real-flag"},
		{"--json", "--priority", "0"},
		{"--json", "--status", "open"},
	} {
		if bdshim.ReadyRoutable(args) {
			t.Fatalf("precondition: ReadyRoutable(%v) = true, test case is wrong", args)
		}
		if shape, ok := earlyBdExperimentShape("ready", args); ok {
			t.Errorf("earlyBdExperimentShape(ready, %v) approved as %q, want declined",
				args, shape)
		}
	}
}

// The selector must recognize the new shape, or a
// GC_BD_EXPERIMENT_SHAPE_OVERRIDES entry naming it silently invalidates the
// whole config (parseOverrides returns a zero Config on any unknown shape) and
// takes every OTHER shape down with it.
func TestReadyShapeIsKnownToTheSelector(t *testing.T) {
	env := func(k string) string {
		if k == bdexperiment.ShapeOverridesEnv {
			return string(bdexperiment.ShapeReadyJSON) + "=direct"
		}
		return ""
	}
	cfg := bdexperiment.Parse(env)
	if !cfg.Valid {
		t.Fatalf("Parse with %s=direct produced an invalid config; "+
			"knownShape() is missing the shape", bdexperiment.ShapeReadyJSON)
	}
	if got := cfg.Overrides[bdexperiment.ShapeReadyJSON]; got != bdexperiment.ArmDirect {
		t.Errorf("override for %s = %q, want %q",
			bdexperiment.ShapeReadyJSON, got, bdexperiment.ArmDirect)
	}
}
