package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// TestCmdSlingRoundRobinCyclesAcrossTargets drives gc sling end-to-end with
// default_sling_strategy = "round_robin" over two targets and asserts the
// dispatch order cycles deterministically (worker-a then worker-b), proving
// the durable cursor advances across separate sling invocations.
func TestCmdSlingRoundRobinCyclesAcrossTargets(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	t.Setenv("GC_BEADS", "file")

	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_CITY_PATH", "")
	t.Setenv("GC_CITY_ROOT", "")
	t.Setenv("GC_RIG", "")
	t.Setenv("GC_RIG_ROOT", "")
	rigDir := filepath.Join(cityDir, "foundations")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(rig): %v", err)
	}
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatalf("ensureScopedFileStoreLayout: %v", err)
	}
	for _, dir := range []string{cityDir, rigDir} {
		if err := ensurePersistedScopeLocalFileStore(dir); err != nil {
			t.Fatalf("ensurePersistedScopeLocalFileStore(%s): %v", dir, err)
		}
	}
	writeTestFileStoreBeads(t, rigDir, []beads.Bead{
		{ID: "fo-rr-1", Title: "rr one", Type: "task", Status: "open", Metadata: map[string]string{}},
		{ID: "fo-rr-2", Title: "rr two", Type: "task", Status: "open", Metadata: map[string]string{}},
	})

	cityToml := `[workspace]
name = "demo"

[[rigs]]
name = "foundations"
path = "foundations"
prefix = "fo"
default_sling_targets = ["foundations/worker-a", "foundations/worker-b"]
default_sling_strategy = "round_robin"

[[agent]]
name = "worker-a"
dir = "foundations"

[[agent]]
name = "worker-b"
dir = "foundations"
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	t.Chdir(cityDir)

	routeOf := func(bead string) string {
		var stdout, stderr bytes.Buffer
		code := cmdSling(
			[]string{bead},
			false, false, false,
			"", nil, "",
			true, false, false, "",
			false, false, false,
			"", "",
			&stdout, &stderr,
		)
		if code != 0 {
			t.Fatalf("cmdSling(%s) = %d; stderr=%s", bead, code, stderr.String())
		}
		rigStore, err := openStoreAtForCity(rigDir, cityDir)
		if err != nil {
			t.Fatalf("openStoreAtForCity: %v", err)
		}
		routed, err := rigStore.Get(bead)
		if err != nil {
			t.Fatalf("Get(%s): %v", bead, err)
		}
		return routed.Metadata["gc.routed_to"]
	}

	if first := routeOf("fo-rr-1"); first != "foundations/worker-a" {
		t.Fatalf("first sling routed_to = %q, want foundations/worker-a", first)
	}
	if second := routeOf("fo-rr-2"); second != "foundations/worker-b" {
		t.Fatalf("second sling routed_to = %q, want foundations/worker-b (cursor did not advance)", second)
	}
}

func TestSelectDefaultSlingTarget_RoundRobinCyclesAndWraps(t *testing.T) {
	city := t.TempDir()
	rig := config.Rig{
		Name:                 "demo",
		DefaultSlingTargets:  []string{"demo/a", "demo/b", "demo/c"},
		DefaultSlingStrategy: "round_robin",
	}
	want := []string{"demo/a", "demo/b", "demo/c", "demo/a", "demo/b", "demo/c", "demo/a"}
	for i, exp := range want {
		got, err := selectDefaultSlingTarget(rig, city)
		if err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
		if got != exp {
			t.Fatalf("call %d: got %q, want %q (cursor not cycling evenly)", i, got, exp)
		}
	}
}

func TestSelectDefaultSlingTarget_RoundRobinSurvivesTargetCountChange(t *testing.T) {
	city := t.TempDir()
	rig := config.Rig{Name: "demo", DefaultSlingTargets: []string{"demo/a", "demo/b", "demo/c"}, DefaultSlingStrategy: "round_robin"}
	// Advance cursor to 2.
	_, _ = selectDefaultSlingTarget(rig, city)
	_, _ = selectDefaultSlingTarget(rig, city)
	// Shrink the target list; cursor (now 2) % 2 must stay in range, not panic.
	rig.DefaultSlingTargets = []string{"demo/a", "demo/b"}
	got, err := selectDefaultSlingTarget(rig, city)
	if err != nil {
		t.Fatalf("unexpected error after shrink: %v", err)
	}
	if got != "demo/a" {
		t.Fatalf("got %q, want demo/a (cursor 2 %% 2 == 0)", got)
	}
}

func TestSelectDefaultSlingTarget_RandomDefaultStaysInSet(t *testing.T) {
	city := t.TempDir()
	set := map[string]bool{"demo/a": true, "demo/b": true}
	for _, strategy := range []string{"", "random"} {
		rig := config.Rig{Name: "demo", DefaultSlingTargets: []string{"demo/a", "demo/b"}, DefaultSlingStrategy: strategy}
		for i := 0; i < 20; i++ {
			got, err := selectDefaultSlingTarget(rig, city)
			if err != nil {
				t.Fatalf("strategy %q: unexpected error: %v", strategy, err)
			}
			if !set[got] {
				t.Fatalf("strategy %q: got %q outside target set", strategy, got)
			}
		}
	}
}

func TestSelectDefaultSlingTarget_InvalidStrategyErrors(t *testing.T) {
	rig := config.Rig{Name: "demo", DefaultSlingTargets: []string{"demo/a"}, DefaultSlingStrategy: "bogus"}
	if _, err := selectDefaultSlingTarget(rig, t.TempDir()); err == nil {
		t.Fatal("expected error for invalid strategy, got nil")
	}
}

func TestSelectDefaultSlingTarget_EmptyEntryErrors(t *testing.T) {
	rig := config.Rig{Name: "demo", DefaultSlingTargets: []string{"demo/a", "  "}}
	if _, err := selectDefaultSlingTarget(rig, t.TempDir()); err == nil {
		t.Fatal("expected error for empty target entry, got nil")
	}
}

func TestSelectDefaultSlingTarget_SingularFallback(t *testing.T) {
	rig := config.Rig{Name: "demo", DefaultSlingTarget: "demo/solo"}
	got, err := selectDefaultSlingTarget(rig, t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "demo/solo" {
		t.Fatalf("got %q, want demo/solo", got)
	}
}

func TestSelectDefaultSlingTarget_NoneConfiguredErrors(t *testing.T) {
	rig := config.Rig{Name: "demo"}
	if _, err := selectDefaultSlingTarget(rig, t.TempDir()); err == nil {
		t.Fatal("expected error when no targets configured, got nil")
	}
}
