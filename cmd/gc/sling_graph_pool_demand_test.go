package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestOnFormulaGraphV2UsesOnePoolDemandUnit(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	maxWorkers := 2
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	applyFeatureFlags(cfg)
	t.Cleanup(func() { applyFeatureFlags(&config.City{}) })
	dir := testFormulaDir(t)
	cfg.FormulaLayers.City = []string{dir}
	graphFormula := `
formula = "graph-work"
version = 2
contract = "graph.v2"

[[steps]]
id = "step"
title = "Do work"
`
	if err := os.WriteFile(filepath.Join(dir, "graph-work.toml"), []byte(graphFormula), 0o644); err != nil {
		t.Fatal(err)
	}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	store := beads.NewMemStoreFrom(1, []beads.Bead{
		{ID: "BL-42", Title: "Work", Type: "task", Status: "open", Metadata: map[string]string{"gc.routed_to": "worker"}},
	}, nil)
	deps.Store = store
	config.InjectImplicitAgents(cfg)
	addTestControlDispatcherAgents(cfg, "")

	target := config.Agent{Name: "worker", MaxActiveSessions: &maxWorkers}
	opts := testOpts(target, "BL-42")
	opts.OnFormula = "graph-work"
	opts.ScopeKind = "city"
	opts.ScopeRef = "test-city"
	if code := doSling(opts, deps, nil, stdout, stderr); code != 0 {
		t.Fatalf("doSling returned %d, want 0; stderr: %s", code, stderr.String())
	}

	source, err := store.Get("BL-42")
	if err != nil {
		t.Fatal(err)
	}
	if got := source.Metadata["gc.routed_to"]; got != "" {
		t.Fatalf("source gc.routed_to = %q, want empty", got)
	}
	if got := source.Metadata["gc.execution_routed_to"]; got != "worker" {
		t.Fatalf("source gc.execution_routed_to = %q, want worker", got)
	}

	counts, partials, errs := defaultScaleCheckCounts([]defaultScaleCheckTarget{
		defaultScaleCheckTargetForAgent(sharedTestCityDir, cfg, &target, store, nil),
	})
	if len(errs) != 0 || len(partials) != 0 {
		t.Fatalf("scale check errors=%v partials=%v", errs, partials)
	}
	if got := counts["worker"]; got != 1 {
		t.Fatalf("pool demand = %d, want 1", got)
	}
}
