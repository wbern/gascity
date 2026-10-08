package config

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// A group must be defined with a nonnegative capacity before it can be used.
func TestCapacityGroupsValidation(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		invalid    bool
	}{
		{"valid", "[workspace.capacity_groups]\nshared=3\n[[agent]]\nname='one'\ncapacity_group='shared'", false},
		{"zero", "[workspace.capacity_groups]\nshared=0\n[[agent]]\nname='one'\ncapacity_group='shared'", false},
		{"negative", "[workspace.capacity_groups]\nshared=-1", true},
		{"unknown", "[[agent]]\nname='one'\ncapacity_group='missing'", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.body))
			if (err != nil) != tc.invalid {
				t.Fatalf("Parse error=%v, invalid=%v", err, tc.invalid)
			}
		})
	}
}

func TestCapacityGroupPatchAndOverride(t *testing.T) {
	a := Agent{Name: "one", CapacityGroup: "old"}
	group := "shared"
	applyAgentPatchFields(&a, &AgentPatch{CapacityGroup: &group})
	if a.CapacityGroup != group {
		t.Fatal("patch did not apply")
	}
	group = ""
	applyAgentOverride(&a, &AgentOverride{CapacityGroup: &group})
	if a.CapacityGroup != "" {
		t.Fatal("override did not clear group")
	}
	if a.Clone().CapacityGroup != a.CapacityGroup {
		t.Fatal("clone lost group")
	}
}

// Workspace fragments must merge group keys without aliasing either input map.
func TestCapacityGroupsFragmentCopy(t *testing.T) {
	base := &City{Workspace: Workspace{CapacityGroups: map[string]int{"x": 3, "y": 2}}}
	fragment, md, _, err := parseWithMeta([]byte("[workspace.capacity_groups]\nx=1\nz=0"), "fragment.toml")
	if err != nil {
		t.Fatal(err)
	}
	old := base.Workspace.CapacityGroups
	mergeWorkspace(base, fragment, md, "fragment.toml", &Provenance{Workspace: map[string]string{}})
	if base.Workspace.CapacityGroups["x"] != 1 || base.Workspace.CapacityGroups["y"] != 2 || base.Workspace.CapacityGroups["z"] != 0 {
		t.Fatal(base.Workspace.CapacityGroups)
	}
	base.Workspace.CapacityGroups["x"] = 9
	if old["x"] != 3 || fragment.Workspace.CapacityGroups["x"] != 1 {
		t.Fatal("merged map aliases input")
	}
}

// Validate effective membership after fragments and patches, rather than rejecting valid split definitions.
func TestCapacityGroupsComposedValidation(t *testing.T) {
	for _, group := range []string{"shared", "unknown"} {
		fs := fsys.NewFake()
		fs.Files["/city/city.toml"] = []byte("include=['groups.toml']\n[workspace]\nname='test'\n[[agent]]\nname='one'\n[[patches.agent]]\nname='one'\ncapacity_group='" + group + "'")
		fs.Files["/city/groups.toml"] = []byte("[workspace.capacity_groups]\nshared=3")
		cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
		if group == "unknown" {
			if err == nil {
				t.Fatal("unknown effective group accepted")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, a := range cfg.Agents {
			if a.Name == "one" {
				found = true
				if a.CapacityGroup != "shared" {
					t.Fatalf("membership=%q", a.CapacityGroup)
				}
			}
		}
		if !found {
			t.Fatal("agent missing")
		}
	}
}

func TestCapacityGroupPatchWireOmission(t *testing.T) {
	absent, err := json.Marshal(AgentPatch{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(absent), "CapacityGroup") {
		t.Fatal("unset group must remain optional on the patch wire")
	}
	empty := ""
	clearPatch, err := json.Marshal(AgentPatch{CapacityGroup: &empty})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(clearPatch), `"CapacityGroup":""`) {
		t.Fatalf("explicit clear lost: %s", clearPatch)
	}
}
