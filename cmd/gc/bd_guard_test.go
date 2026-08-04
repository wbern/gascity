package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestBdGuardRefusalAllowsRigTargetsAndRefusesHQAliases(t *testing.T) {
	city := filepath.Join(resolvedTempDir(t), "city")
	alias := filepath.Join(filepath.Dir(city), ".", filepath.Base(city))
	cases := []struct {
		name      string
		target    execStoreTarget
		guardCity string
		refuse    bool
	}{
		{"own rig", execStoreTarget{ScopeKind: "rig", ScopeRoot: filepath.Join(city, "own")}, city, false},
		{"foreign rig", execStoreTarget{ScopeKind: "rig", ScopeRoot: filepath.Join(city, "foreign")}, city, false},
		{"city", execStoreTarget{ScopeKind: "city", ScopeRoot: city}, city, true},
		{"equivalent city spelling", execStoreTarget{ScopeKind: "city", ScopeRoot: alias}, city, true},
		{"marker execution disagreement", execStoreTarget{ScopeKind: "rig", ScopeRoot: filepath.Join(city, "own")}, filepath.Join(city, "different"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg, refuse := bdGuardRefusal(tc.guardCity, city, tc.target)
			if refuse != tc.refuse {
				t.Fatalf("bdGuardRefusal() = (%q, %v), want refuse=%v", msg, refuse, tc.refuse)
			}
		})
	}
}

func TestDoBdGuardRefusesExplicitCityBeforeOutputOrSubprocess(t *testing.T) {
	disableManagedDoltRecoveryForTest(t)

	origCityFlag, origRigFlag := cityFlag, rigFlag
	origProbe := bdBeadExists
	t.Cleanup(func() {
		cityFlag = origCityFlag
		rigFlag = origRigFlag
		bdBeadExists = origProbe
	})
	cityFlag, rigFlag = "", ""

	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "rig")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"
prefix = "hq"

[[rigs]]
name = "rig"
path = "rig"
prefix = "rg"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	bdBeadExists = func(_ string, _ *config.City, target execStoreTarget, id string) bool {
		return target.ScopeKind == "city" && id == "hq-1"
	}
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte("#!/bin/sh\nprintf SHOULD_NOT_RUN\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv(bdGuardMarkerEnv, bdGuardMarkerValue)
	t.Setenv(bdGuardCityEnv, cityDir)
	t.Setenv("GC_RIG", "rig")

	for _, args := range [][]string{
		{"--city", cityDir, "list"},
		{"--city", cityDir, "list", "--json"},
		{"show", "hq-1", "--json"},
		{"list", "-C", cityDir},
	} {
		var stdout, stderr bytes.Buffer
		if code := doBd(args, &stdout, &stderr); code != 1 {
			t.Fatalf("doBd(%v) code = %d, want 1", args, code)
		}
		if stdout.Len() != 0 {
			t.Fatalf("doBd(%v) stdout = %q, want empty", args, stdout.String())
		}
		if got := stderr.String(); !strings.Contains(got, "refusing city (HQ) store") {
			t.Fatalf("doBd(%v) stderr = %q, want HQ refusal", args, got)
		}
	}
}

func TestResolveBdScopeTargetTreatsEffectiveDirectoryCityAsCityBeforeGCRig(t *testing.T) {
	cityDir := filepath.Join(resolvedTempDir(t), "city")
	rigDir := filepath.Join(cityDir, "rig")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Rigs:      []config.Rig{{Name: "rig", Path: rigDir, Prefix: "rg"}},
	}
	t.Setenv("GC_RIG", "rig")

	target, err := resolveBdScopeTarget(cfg, cityDir, "", []string{"list", "-C", filepath.Join(cityDir, ".")}, false, nil)
	if err != nil {
		t.Fatalf("resolveBdScopeTarget: %v", err)
	}
	if target.ScopeKind != "city" || !samePath(target.ScopeRoot, cityDir) {
		t.Fatalf("target = %#v, want city target rooted at %q", target, cityDir)
	}
}

func TestDoBdGuardAllowsOwnAndForeignRigStores(t *testing.T) {
	disableManagedDoltRecoveryForTest(t)

	origCityFlag, origRigFlag := cityFlag, rigFlag
	t.Cleanup(func() {
		cityFlag = origCityFlag
		rigFlag = origRigFlag
	})
	cityFlag, rigFlag = "", ""

	cityDir := t.TempDir()
	for _, name := range []string{"own", "foreign"} {
		if err := os.MkdirAll(filepath.Join(cityDir, name, ".beads"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[[rigs]]
name = "own"
path = "own"
prefix = "ow"

[[rigs]]
name = "foreign"
path = "foreign"
prefix = "fr"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte("#!/bin/sh\nprintf 'rig-ok\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv(bdGuardMarkerEnv, bdGuardMarkerValue)
	t.Setenv(bdGuardCityEnv, cityDir)

	for _, rig := range []string{"own", "foreign"} {
		var stdout, stderr bytes.Buffer
		if code := doBd([]string{"--rig", rig, "list"}, &stdout, &stderr); code != 0 {
			t.Fatalf("doBd(--rig %s) code = %d, stderr=%q", rig, code, stderr.String())
		}
		if got := stdout.String(); got != "rig-ok\n" {
			t.Fatalf("doBd(--rig %s) stdout = %q, want rig-ok", rig, got)
		}
	}
}
