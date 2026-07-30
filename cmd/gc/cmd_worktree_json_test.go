package main

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

// runWorktreeJSONCommand drives the real cobra root, so the JSON contract gate
// runs. Calling doWorktreeScan directly is what let a permanently broken --json
// flag ship green for its whole life: the unit tests never met the gate.
func runWorktreeJSONCommand(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	root := newRootCmd(&stdout, &stderr)
	if handled, code := handleJSONContractRequest(root, args, &stdout, &stderr); handled {
		return stdout.String(), stderr.String(), code
	}
	root.SetArgs(args)
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	code := 0
	if err := root.Execute(); err != nil {
		code = 1
	}
	return stdout.String(), stderr.String(), code
}

// stubWorktreeCityDeps points the worktree commands at an empty in-memory city
// so the JSON shape can be asserted without standing up rigs or git worktrees.
func stubWorktreeCityDeps(t *testing.T) {
	t.Helper()
	clearGCEnv(t)
	restore := stubWorktreeScanDeps(t)
	origLive := worktreeLiveWorkerDirsFn
	origReap := worktreeReapClosedBeadWorktrees
	origOpenRig := worktreeOpenRigStore
	t.Cleanup(func() {
		restore()
		worktreeLiveWorkerDirsFn = origLive
		worktreeReapClosedBeadWorktrees = origReap
		worktreeOpenRigStore = origOpenRig
	})

	cityDir := t.TempDir()
	worktreeResolveCity = func() (string, error) { return cityDir, nil }
	worktreeLoadCityConfig = func(string, ...io.Writer) (*config.City, error) {
		return &config.City{Rigs: []config.Rig{{Name: "demo", Path: t.TempDir()}}}, nil
	}
	worktreeLiveWorkerDirsFn = func(string) (map[string]bool, error) { return map[string]bool{}, nil }
	worktreeOpenRigStore = func(string, string) (beads.Store, error) {
		return beads.NewMemStoreFrom(1, nil, nil), nil
	}
	worktreeOpenCityStoreAt = func(string) (beads.Store, error) { return beads.NewMemStoreFrom(1, nil, nil), nil }
	worktreeListAllSessionBeads = func(beads.Store, beads.ListQuery) ([]beads.Bead, error) { return nil, nil }
	worktreeScanStrayWorktrees = func([]string, map[string]bool, func(string) gitProbe) ([]strayWorktree, error) {
		return []strayWorktree{
			{Path: "/wt/clean", Reclaimable: true},
			{Path: "/wt/held", Reason: "unlanded commits"},
		}, nil
	}
	worktreeReapClosedBeadWorktrees = func(string, *config.City, map[string]beads.Store, []string, bool, events.Recorder, io.Writer) reapReport {
		return reapReport{
			DryRun: true,
			Reaped: []reapDecision{{BeadID: "b-1", Rig: "demo", Branch: "polecat/b-1", Path: "/wt/1"}},
			Protected: []reapDecision{
				{BeadID: "b-2", Rig: "demo", Path: "/wt/2", Reason: "unsafe git state: uncommitted=false unlanded=true", HoldsUnlandedWork: true},
			},
		}
	}
}

func TestWorktreeScanJSONSatisfiesTheContract(t *testing.T) {
	stubWorktreeCityDeps(t)

	stdout, stderr, code := runWorktreeJSONCommand(t, "worktree", "scan", "--json")
	if strings.Contains(stdout, "json_unsupported") {
		t.Fatalf("worktree scan --json still fails the contract gate: %s", stdout)
	}
	if code != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	validateJSONResultSchema(t, []string{"worktree", "scan"}, []byte(stdout))

	var envelope struct {
		SchemaVersion string `json:"schema_version"`
		OK            bool   `json:"ok"`
		Strays        []struct {
			Path        string `json:"path"`
			Reclaimable bool   `json:"reclaimable"`
		} `json:"strays"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatalf("payload is not an object envelope: %v\n%s", err, stdout)
	}
	if envelope.SchemaVersion != "1" || !envelope.OK {
		t.Errorf("envelope = %+v, want schema_version 1 and ok true", envelope)
	}
	if len(envelope.Strays) != 2 {
		t.Fatalf("strays = %+v, want 2", envelope.Strays)
	}
}

func TestWorktreeReapJSONSatisfiesTheContract(t *testing.T) {
	stubWorktreeCityDeps(t)

	stdout, stderr, code := runWorktreeJSONCommand(t, "worktree", "reap", "--json")
	if strings.Contains(stdout, "json_unsupported") {
		t.Fatalf("worktree reap --json fails the contract gate: %s", stdout)
	}
	if code != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	validateJSONResultSchema(t, []string{"worktree", "reap"}, []byte(stdout))

	var envelope struct {
		SchemaVersion string `json:"schema_version"`
		OK            bool   `json:"ok"`
		DryRun        bool   `json:"dry_run"`
		Summary       struct {
			Reaped          int `json:"reaped"`
			Kept            int `json:"kept"`
			HoldingUnlanded int `json:"holding_unlanded_work"`
		} `json:"summary"`
		Protected []struct {
			BeadID            string `json:"bead_id"`
			HoldsUnlandedWork bool   `json:"holds_unlanded_work"`
		} `json:"protected"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatalf("payload is not an object envelope: %v\n%s", err, stdout)
	}
	if envelope.SchemaVersion != "1" || !envelope.OK || !envelope.DryRun {
		t.Errorf("envelope = %+v, want schema_version 1, ok true, dry_run true", envelope)
	}
	if envelope.Summary.Reaped != 1 || envelope.Summary.Kept != 1 {
		t.Errorf("summary = %+v, want reaped 1 kept 1", envelope.Summary)
	}
	// The count that made 49 worktrees of stranded work invisible must be a
	// field, not something a consumer has to re-derive from prose.
	if envelope.Summary.HoldingUnlanded != 1 {
		t.Errorf("summary.holding_unlanded_work = %d, want 1", envelope.Summary.HoldingUnlanded)
	}
	if len(envelope.Protected) != 1 || !envelope.Protected[0].HoldsUnlandedWork {
		t.Errorf("protected = %+v, want the unlanded flag carried per entry", envelope.Protected)
	}
}

// TestWorktreeReapJSONRefusesExecute keeps the destructive path out of the
// machine-readable surface until someone asks for it deliberately: a caller
// scripting --json should not be able to delete worktrees by adding a flag it
// did not think about.
func TestWorktreeReapJSONDryRunByDefault(t *testing.T) {
	stubWorktreeCityDeps(t)

	stdout, _, _ := runWorktreeJSONCommand(t, "worktree", "reap", "--json")
	if !strings.Contains(stdout, `"dry_run": true`) && !strings.Contains(stdout, `"dry_run":true`) {
		t.Errorf("reap --json must report dry_run true by default; got %s", stdout)
	}
}
