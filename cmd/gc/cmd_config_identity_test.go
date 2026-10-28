package main

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/suspensionstate"

	"github.com/gastownhall/gascity/internal/config"
)

func TestConfigShowIdentitySnapshot(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		cfg := &config.City{}
		cfg.Workspace.CapacityGroups = map[string]int{"core/implementation": 0}
		cfg.Agents = []config.Agent{
			{Dir: "core", Name: "legacy", Provider: "same", CapacityGroup: "core/implementation", Suspended: true, Nudge: "unbound"},
			{Dir: "core", Name: "legacy", BindingName: "pool", Provider: "same", CapacityGroup: "core/implementation", Suspended: false, Nudge: "bound"},
		}
		if reverse {
			cfg.Agents[0], cfg.Agents[1] = cfg.Agents[1], cfg.Agents[0]
		}
		var out bytes.Buffer
		if code := writeConfigShowJSON(&out, "/city", cfg, nil, nil, nil, nil); code != 0 {
			t.Fatal(code)
		}
		var payload struct {
			Config config.City `json:"config"`
			Agents []struct {
				QualifiedName string       `json:"qualified_name"`
				Agent         config.Agent `json:"agent"`
			} `json:"resolved_agents"`
		}
		if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		if len(payload.Agents) != 2 {
			t.Fatalf("missing authoritative identity projection: %s", out.Bytes())
		}
		seen := map[string]config.Agent{}
		for _, r := range payload.Agents {
			seen[r.QualifiedName] = r.Agent
		}
		if len(seen) != 2 || seen["core/legacy"].Nudge != "unbound" || !seen["core/legacy"].Suspended || seen["core/pool.legacy"].Nudge != "bound" || seen["core/pool.legacy"].Suspended {
			t.Fatalf("identity/policy correspondence lost: %+v", seen)
		}
		if payload.Config.Workspace.CapacityGroups["core/implementation"] != 0 {
			t.Fatal("zero shared budget changed")
		}
		raw, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte("BindingName")) {
			t.Fatal("runtime binding leaked into persisted Agent JSON")
		}
		validateConfigShowJSONSchema(t, out.Bytes())
	}
}

func TestConfigShowIdentitySnapshotEmpty(t *testing.T) {
	var out bytes.Buffer
	if code := writeConfigShowJSON(&out, "/city", &config.City{}, nil, nil, nil, nil); code != 0 {
		t.Fatal(code)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if string(payload["resolved_agents"]) != "[]" {
		t.Fatalf("empty projection must be explicit array, got %s", payload["resolved_agents"])
	}
}

func TestConfigShowIdentityRuntimeSuspension(t *testing.T) {
	dir := t.TempDir()
	suspended := true
	if err := suspensionstate.Save(fsys.OSFS{}, dir, suspensionstate.State{City: suspensionstate.Override{Suspended: &suspended}}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{Agents: []config.Agent{{Name: "worker", Provider: "codex"}}}
	var out bytes.Buffer
	if code := writeConfigShowJSON(&out, dir, cfg, nil, nil, nil, nil); code != 0 {
		t.Fatal(code)
	}
	var payload struct {
		Agents []struct {
			Suspended *bool        `json:"suspended"`
			Agent     config.Agent `json:"agent"`
		} `json:"resolved_agents"`
	}
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Agents) != 1 || payload.Agents[0].Suspended == nil || !*payload.Agents[0].Suspended || payload.Agents[0].Agent.Suspended {
		t.Fatal("runtime suspension not captured separately from authored policy")
	}
	if err := os.WriteFile(citylayout.SuspensionStateFile(dir), []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := writeConfigShowJSON(&out, dir, cfg, nil, nil, nil, nil); code == 0 {
		t.Fatal("unreadable suspension identity must fail closed")
	}
}
