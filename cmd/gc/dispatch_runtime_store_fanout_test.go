package main

import (
	"io"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// TestDrainWorkflowServeWorkSelectsRigStoreControlBead is the regression test
// for the #3764 residual: the control-dispatcher singleton's serve loop must
// select workflow-finalize (and other) control beads that are routed to it
// but physically materialized in a RIG store, not just the city store. Before
// the fix, drainWorkflowServeWork only ever queried storePath (== cityPath
// for the singleton), so a rig-store finalize bead was never selected and its
// graph.v2 molecule root stayed in_progress forever.
func TestDrainWorkflowServeWorkSelectsRigStoreControlBead(t *testing.T) {
	prevList := workflowServeList
	prevControl := controlDispatcherServe
	t.Cleanup(func() {
		workflowServeList = prevList
		controlDispatcherServe = prevControl
	})

	cityPath := t.TempDir()
	rigPath := t.TempDir()

	rigFinalizeBead := hookBead{ID: "rig-finalize-1", Metadata: map[string]string{"gc.kind": "workflow-finalize"}}

	var queriedStores []string
	rigServed := false
	workflowServeList = func(_, storePath string, _ map[string]string) ([]hookBead, error) {
		queriedStores = append(queriedStores, storePath)
		if storePath == rigPath && !rigServed {
			rigServed = true
			return []hookBead{rigFinalizeBead}, nil
		}
		return nil, nil
	}

	var processed []struct{ beadID, storePath string }
	controlDispatcherServe = func(_, storePath, beadID string, _, _ io.Writer) error {
		processed = append(processed, struct{ beadID, storePath string }{beadID, storePath})
		return nil
	}

	agentCfg := config.Agent{Name: config.ControlDispatcherAgentName}
	cfg := &config.City{Rigs: []config.Rig{{Name: "gas-city-wbern", Path: rigPath}}}

	result, err := drainWorkflowServeWork(agentCfg, cityPath, cityPath, agentCfg.EffectiveWorkQuery(), nil, cfg, io.Discard)
	if err != nil {
		t.Fatalf("drainWorkflowServeWork error = %v", err)
	}
	if !result.processedAny {
		t.Fatalf("result.processedAny = false, want true (rig-store finalize bead should have been processed)")
	}

	if len(processed) != 1 || processed[0].beadID != "rig-finalize-1" || processed[0].storePath != rigPath {
		t.Fatalf("processed = %#v, want exactly one call for rig-finalize-1 against store %q", processed, rigPath)
	}

	var sawCity, sawRig bool
	for _, sp := range queriedStores {
		if sp == cityPath {
			sawCity = true
		}
		if sp == rigPath {
			sawRig = true
		}
	}
	if !sawCity || !sawRig {
		t.Fatalf("queriedStores = %#v, want both city store %q and rig store %q scanned", queriedStores, cityPath, rigPath)
	}
}

// TestDrainWorkflowServeWorkNonSingletonAgentStaysSingleStore pins the
// no-regression contract: an ordinary (non control-dispatcher) agent must
// keep querying only its own storePath, even when the city config has rig
// stores configured.
func TestDrainWorkflowServeWorkNonSingletonAgentStaysSingleStore(t *testing.T) {
	prevList := workflowServeList
	prevControl := controlDispatcherServe
	t.Cleanup(func() {
		workflowServeList = prevList
		controlDispatcherServe = prevControl
	})

	cityPath := t.TempDir()
	rigPath := t.TempDir()

	var queriedStores []string
	workflowServeList = func(_, storePath string, _ map[string]string) ([]hookBead, error) {
		queriedStores = append(queriedStores, storePath)
		return nil, nil
	}
	controlDispatcherServe = func(_, _, beadID string, _, _ io.Writer) error {
		t.Fatalf("controlDispatcherServe unexpectedly called for bead %s", beadID)
		return nil
	}

	agentCfg := config.Agent{Name: "worker"}
	cfg := &config.City{Rigs: []config.Rig{{Name: "gas-city-wbern", Path: rigPath}}}

	if _, err := drainWorkflowServeWork(agentCfg, cityPath, cityPath, agentCfg.EffectiveWorkQuery(), nil, cfg, io.Discard); err != nil {
		t.Fatalf("drainWorkflowServeWork error = %v", err)
	}

	if len(queriedStores) != 1 || queriedStores[0] != cityPath {
		t.Fatalf("queriedStores = %#v, want exactly [%q] for a non-singleton agent", queriedStores, cityPath)
	}
}
