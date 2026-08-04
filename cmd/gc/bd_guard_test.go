package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestBdGuardRefusalRequiresPositiveHQAuthorization(t *testing.T) {
	city := filepath.Join(resolvedTempDir(t), "city")
	alias := filepath.Join(filepath.Dir(city), ".", filepath.Base(city))
	cases := []struct {
		name      string
		target    execStoreTarget
		guardCity string
		access    string
		refuse    bool
	}{
		{"own rig without HQ access", execStoreTarget{ScopeKind: "rig", ScopeRoot: filepath.Join(city, "own")}, city, "", false},
		{"foreign rig without HQ access", execStoreTarget{ScopeKind: "rig", ScopeRoot: filepath.Join(city, "foreign")}, city, "", false},
		{"city without HQ access", execStoreTarget{ScopeKind: "city", ScopeRoot: city}, city, "", true},
		{"equivalent city spelling without HQ access", execStoreTarget{ScopeKind: "city", ScopeRoot: alias}, city, "", true},
		{"city with HQ access", execStoreTarget{ScopeKind: "city", ScopeRoot: city}, city, bdGuardMarkerValue, false},
		{"marker execution disagreement fails even with access", execStoreTarget{ScopeKind: "rig", ScopeRoot: filepath.Join(city, "own")}, filepath.Join(city, "different"), bdGuardMarkerValue, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg, refuse := bdGuardRefusal(tc.guardCity, city, tc.access, tc.target)
			if refuse != tc.refuse {
				t.Fatalf("bdGuardRefusal() = (%q, %v), want refuse=%v", msg, refuse, tc.refuse)
			}
		})
	}
}

func TestBdGuardDirectoryMustSelectResolvedStore(t *testing.T) {
	city := filepath.Join(resolvedTempDir(t), "city")
	own := filepath.Join(city, "own")
	foreign := filepath.Join(city, "foreign")
	neutral := filepath.Join(city, "neutral")
	outside := filepath.Join(resolvedTempDir(t), "outside")
	for _, dir := range []string{own, foreign, neutral, outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo", Prefix: "hq"},
		Rigs: []config.Rig{
			{Name: "own", Path: own, Prefix: "ow"},
			{Name: "foreign", Path: foreign, Prefix: "fr"},
		},
	}
	cases := []struct {
		name      string
		directory string
		target    execStoreTarget
		refuse    bool
	}{
		{"rig subdirectory agrees", filepath.Join(own, "subdir"), bdRigScopeTarget(city, cfg.Rigs[0]), false},
		{"other rig disagrees", foreign, bdRigScopeTarget(city, cfg.Rigs[0]), true},
		{"neutral city subdirectory agrees", neutral, bdCityScopeTarget(city, cfg), false},
		{"rig directory disagrees with city", own, bdCityScopeTarget(city, cfg), true},
		{"outside directory is unverifiable", outside, bdRigScopeTarget(city, cfg.Rigs[0]), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg, refuse := bdGuardDirectoryRefusal(cfg, city, tc.directory, tc.target)
			if refuse != tc.refuse {
				t.Fatalf("bdGuardDirectoryRefusal() = (%q, %v), want refuse=%v", msg, refuse, tc.refuse)
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
	t.Setenv(bdGuardAccessEnv, "")
	t.Setenv("GC_ALIAS", "demo/worker-2")
	t.Setenv("GC_AGENT", "demo/worker")
	t.Setenv("GC_RIG", "rig")
	t.Setenv("GC_RIG_ROOT", rigDir)

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
		for _, want := range []string{
			`managed agent "demo/worker-2"`,
			`denied HQ store "` + cityDir + `"`,
			`agent rig "rig"`,
			`gc bd`,
			`--rig rig`,
			`rig store "` + rigDir + `"`,
		} {
			if got := stderr.String(); !strings.Contains(got, want) {
				t.Fatalf("doBd(%v) stderr = %q, want actionable context %q", args, got, want)
			}
		}
	}

	t.Setenv(bdGuardAccessEnv, bdGuardMarkerValue)
	var stdout, stderr bytes.Buffer
	if code := doBd([]string{"--city", cityDir, "list"}, &stdout, &stderr); code != 0 {
		t.Fatalf("authorized doBd(--city) code = %d, stderr=%q", code, stderr.String())
	}
	if got := stdout.String(); got != "SHOULD_NOT_RUN" {
		t.Fatalf("authorized doBd(--city) stdout = %q, want delegated bd output", got)
	}
}

func TestDoBdGuardRefusesDirectoryThatDisagreesWithResolvedRig(t *testing.T) {
	disableManagedDoltRecoveryForTest(t)

	origCityFlag, origRigFlag := cityFlag, rigFlag
	t.Cleanup(func() {
		cityFlag = origCityFlag
		rigFlag = origRigFlag
	})
	cityFlag, rigFlag = "", ""

	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "rig")
	outsideDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[[rigs]]
name = "rig"
path = "rig"
prefix = "rg"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte("#!/bin/sh\nprintf SHOULD_NOT_RUN\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv("GC_RIG", "rig")
	t.Setenv(bdGuardMarkerEnv, bdGuardMarkerValue)
	t.Setenv(bdGuardCityEnv, cityDir)
	t.Setenv(bdGuardAccessEnv, "")

	for _, directoryArg := range [][]string{
		{"-C", outsideDir},
		{"-C=" + outsideDir},
		{"-C" + outsideDir},
		{"--directory", outsideDir},
		{"--directory=" + outsideDir},
	} {
		args := append([]string{"list"}, directoryArg...)
		var stdout, stderr bytes.Buffer
		if code := doBd(args, &stdout, &stderr); code != 1 {
			t.Fatalf("doBd(%v) code = %d, want 1", args, code)
		}
		if stdout.Len() != 0 {
			t.Fatalf("doBd(%v) stdout = %q, want empty", args, stdout.String())
		}
		if got := stderr.String(); !strings.Contains(got, "directory") || !strings.Contains(got, "resolved rig") {
			t.Fatalf("doBd(%v) stderr = %q, want directory/target disagreement", args, got)
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
	t.Setenv(bdGuardAccessEnv, "")

	for _, rig := range []string{"own", "foreign"} {
		var stdout, stderr bytes.Buffer
		if code := doBd([]string{"--rig", rig, "list"}, &stdout, &stderr); code != 0 {
			t.Fatalf("doBd(--rig %s) code = %d, stderr=%q", rig, code, stderr.String())
		}
		if got := stdout.String(); got != "rig-ok\n" {
			t.Fatalf("doBd(--rig %s) stdout = %q, want rig-ok", rig, got)
		}
	}

	rigSubdir := filepath.Join(cityDir, "own", "subdir")
	if err := os.MkdirAll(rigSubdir, 0o755); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := doBd([]string{"--rig", "own", "list", "-C=" + rigSubdir}, &stdout, &stderr); code != 0 {
		t.Fatalf("doBd(--rig own -C subdir) code = %d, stderr=%q", code, stderr.String())
	}
	if got := stdout.String(); got != "rig-ok\n" {
		t.Fatalf("doBd(--rig own -C subdir) stdout = %q, want rig-ok", got)
	}
}
