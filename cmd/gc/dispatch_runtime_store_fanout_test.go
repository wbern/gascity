package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
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

// TestDrainWorkflowServeWorkRigStoreEnvPointsBEADSDIRAtRigStore is the
// regression test for the gcw-tjeg follow-up (gcw-kcm4): the per-store fanout
// added to drainWorkflowServeWork only ever changed the shell CWD (dir=sp)
// per store — it reused the SAME work-query env (built once from the city's
// controllerWorkQueryEnv) for every store, so BEADS_DIR never repointed at
// the rig's actual bead-store dir. This test does NOT stub workflowServeList
// — it runs the real nextWorkflowServeBeads -> shellWorkQueryWithEnv path
// with a work_query shell command that only reports ready work when it
// observes BEADS_DIR pointing at the rig's store, so the query itself proves
// which store's env reached the subprocess. It must FAIL before the fix
// (BEADS_DIR stays at the city store for every iteration) and pass after.
func TestDrainWorkflowServeWorkRigStoreEnvPointsBEADSDIRAtRigStore(t *testing.T) {
	prevControl := controlDispatcherServe
	t.Cleanup(func() { controlDispatcherServe = prevControl })

	cityPath := t.TempDir()
	rigPath := t.TempDir()
	rigBeadsDir := filepath.Join(rigPath, ".beads")
	// donePath models the bead leaving "ready" once processed: real bead
	// stores stop returning a bead once its control step advances, but this
	// test's work_query is a stateless shell script, so it needs an external
	// marker or it re-reports the same bead forever and the drain loop (which
	// continues polling after every processed cycle) never sees an empty
	// queue and never returns.
	donePath := filepath.Join(t.TempDir(), "done")

	// work_query: report the finalize bead only when the subprocess sees
	// BEADS_DIR pointed at the rig's own store AND the bead has not yet been
	// marked done, proving the env (not just the cwd) was repointed for this
	// store's iteration.
	workQuery := fmt.Sprintf(
		`if [ "$BEADS_DIR" = %q ] && [ ! -e %q ]; then echo '[{"id":"rig-finalize-1","metadata":{"gc.kind":"workflow-finalize"}}]'; else echo '[]'; fi`,
		rigBeadsDir, donePath,
	)

	var processed []struct{ beadID, storePath string }
	controlDispatcherServe = func(_, storePath, beadID string, _, _ io.Writer) error {
		processed = append(processed, struct{ beadID, storePath string }{beadID, storePath})
		if err := os.WriteFile(donePath, nil, 0o644); err != nil {
			t.Fatalf("marking bead done: %v", err)
		}
		return nil
	}

	agentCfg := config.Agent{Name: config.ControlDispatcherAgentName}
	cfg := &config.City{Rigs: []config.Rig{{Name: "gas-city-wbern", Path: rigPath}}}
	// workEnv mirrors what controllerWorkQueryEnv builds for the singleton at
	// city scope: BEADS_DIR pinned at the CITY store. If the per-store fanout
	// fails to repoint it, every iteration (including the rig one) sees this
	// city value and the rig work_query branch never fires.
	workEnv := map[string]string{"BEADS_DIR": filepath.Join(cityPath, ".beads")}

	result, err := drainWorkflowServeWork(agentCfg, cityPath, cityPath, workQuery, workEnv, cfg, io.Discard)
	if err != nil {
		t.Fatalf("drainWorkflowServeWork error = %v", err)
	}
	if !result.processedAny {
		t.Fatalf("result.processedAny = false, want true (rig-store finalize bead should have been selected via a rig-scoped BEADS_DIR)")
	}
	if len(processed) != 1 || processed[0].beadID != "rig-finalize-1" || processed[0].storePath != rigPath {
		t.Fatalf("processed = %#v, want exactly one call for rig-finalize-1 against store %q", processed, rigPath)
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
