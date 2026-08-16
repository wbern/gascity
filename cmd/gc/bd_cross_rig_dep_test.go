package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func crossRigTestCity() *config.City {
	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "crm", Prefix: "crm"},
			{Name: "gas-city-infra", Prefix: "gci"},
		},
	}
	cfg.Workspace.Prefix = "gc2"
	return cfg
}

// THE REGRESSION: this exact shape returned exit 0 with an affirmative
// "Added dependency" line and wrote nothing, on live gc2 (gci-cxfpt).
func TestBdCrossRigDepRefusalRefusesCrossStore(t *testing.T) {
	msg, refuse := bdCrossRigDepRefusal(crossRigTestCity(), []string{"dep", "add", "crm-1gmu7x", "gci-hflf2"})
	if !refuse {
		t.Fatal("a cross-rig dep add must be refused, not forwarded to a silent no-op")
	}
	// The operator must be able to act without opening the source: both
	// stores named, plus what to do instead.
	for _, want := range []string{"crm-1gmu7x", `rig "crm"`, "gci-hflf2", `rig "gas-city-infra"`, "--status blocked"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal must mention %q; got:\n%s", want, msg)
		}
	}
}

func TestBdCrossRigDepRefusalRigVersusHQ(t *testing.T) {
	if _, refuse := bdCrossRigDepRefusal(crossRigTestCity(), []string{"dep", "add", "gc2-aaa", "gci-bbb"}); !refuse {
		t.Fatal("the city HQ store is a separate database too; that pair must be refused")
	}
}

func TestBdCrossRigDepRefusalFlagForm(t *testing.T) {
	if _, refuse := bdCrossRigDepRefusal(crossRigTestCity(), []string{"dep", "add", "crm-1", "--blocked-by", "gci-2"}); !refuse {
		t.Fatal("--blocked-by is an alias for the positional form and must be refused alike")
	}
}

// A global flag before the verb must not walk past the guard.
func TestBdCrossRigDepRefusalGlobalFlagBeforeVerb(t *testing.T) {
	args := []string{"--actor", "bob", "dep", "add", "crm-1", "gci-2"}
	if _, refuse := bdCrossRigDepRefusal(crossRigTestCity(), args); !refuse {
		t.Fatal("a global flag before the subcommand must not bypass the guard")
	}
}

// THE OTHER HALF, AND THE ONE THAT KEEPS THIS SHIPPABLE: everything that is
// not a provably cross-store dep add must pass through untouched. A guard in
// front of every `gc bd dep add` earns its place only if it cannot produce a
// false refusal.
func TestBdCrossRigDepRefusalAllowsEverythingElse(t *testing.T) {
	cfg := crossRigTestCity()
	cases := []struct {
		name string
		args []string
	}{
		{"same rig", []string{"dep", "add", "gci-1", "gci-2"}},
		{"same rig via flag", []string{"dep", "add", "crm-1", "--depends-on=crm-2"}},
		{"both in HQ", []string{"dep", "add", "gc2-1", "gc2-2"}},
		{"unknown prefix on the left", []string{"dep", "add", "zzz-1", "gci-2"}},
		{"unknown prefix on the right", []string{"dep", "add", "gci-1", "zzz-2"}},
		{"external ref", []string{"dep", "add", "gci-1", "external:other:thing"}},
		{"dep list", []string{"dep", "list", "gci-1"}},
		{"unrelated verb", []string{"update", "gci-1", "--status", "open"}},
		{"bulk file form", []string{"dep", "add", "--file", "edges.ndjson"}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if msg, refuse := bdCrossRigDepRefusal(cfg, tt.args); refuse {
				t.Fatalf("must not refuse %v; got:\n%s", tt.args, msg)
			}
		})
	}
}

func TestBdStoreForBeadIDDerivesPrefixWhenUnset(t *testing.T) {
	// Prefix is optional in config; when absent it is derived from the name.
	cfg := &config.City{Rigs: []config.Rig{{Name: "gas-city-infra"}, {Name: "crm"}}}
	got, ok := bdStoreForBeadID(cfg, "gci-abc")
	if !ok || got.key != "rig:gas-city-infra" {
		t.Fatalf("derived prefix lookup failed: got %+v ok=%v", got, ok)
	}
}

func TestBdStoreForBeadIDRejectsNonLocalIDs(t *testing.T) {
	cfg := crossRigTestCity()
	for _, id := range []string{"", "   ", "nodash", "external:proj:cap"} {
		if _, ok := bdStoreForBeadID(cfg, id); ok {
			t.Errorf("id %q must not resolve to a store", id)
		}
	}
}
