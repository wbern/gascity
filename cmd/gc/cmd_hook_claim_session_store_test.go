package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestSessionPointerStoreDirResolvesCityStore asserts that session-pointer
// writes resolve the CITY store from a rig-scoped work-bead store dir, rather
// than targeting the rig store. Session beads live only in the city store, so
// updating them against a rig store misses the bead entirely — surfacing as
// "recording session pointers on session bead <id>: bead not found" whenever a
// pool worker claims a rig-scoped bead. Regressing this (using the work-bead
// dir directly) is the bug this guards.
func TestSessionPointerStoreDirResolvesCityStore(t *testing.T) {
	// Neutralize this process's ambient city env so resolution walks the
	// fixture filesystem (cityForStoreDir prefers GC_CITY*/GC_CITY_ROOT).
	for _, key := range []string{"GC_CITY", "GC_CITY_PATH", "GC_CITY_ROOT"} {
		t.Setenv(key, "")
	}

	city := t.TempDir()
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("name = \"t\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rigStore := filepath.Join(city, "rigs", "crm")
	if err := os.MkdirAll(rigStore, 0o755); err != nil {
		t.Fatal(err)
	}

	got := sessionPointerStoreDir(rigStore)

	// EvalSymlinks tolerates macOS /var -> /private/var tempdir aliasing.
	wantCity, _ := filepath.EvalSymlinks(city)
	gotCity, _ := filepath.EvalSymlinks(got)
	if gotCity != wantCity {
		t.Errorf("sessionPointerStoreDir(%q) = %q, want city dir %q — session pointers must target the city store where session beads live, not the rig store", rigStore, got, city)
	}
}

// TestSessionPointerStoreDirIdempotentOnCityDir asserts a city-scoped work
// claim is unaffected: resolving from the city dir returns the city dir, so
// city-bead claims keep landing on the same (correct) store.
func TestSessionPointerStoreDirIdempotentOnCityDir(t *testing.T) {
	for _, key := range []string{"GC_CITY", "GC_CITY_PATH", "GC_CITY_ROOT"} {
		t.Setenv(key, "")
	}
	city := t.TempDir()
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("name = \"t\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := sessionPointerStoreDir(city)

	wantCity, _ := filepath.EvalSymlinks(city)
	gotCity, _ := filepath.EvalSymlinks(got)
	if gotCity != wantCity {
		t.Errorf("sessionPointerStoreDir(%q) = %q, want %q (city dir must be idempotent)", city, got, city)
	}
}

// TestSessionPointerStoreEnvClearsRigScope guards the ENV half of the fix — the
// part the dir-only tests above miss. The prior fix repointed only the dir but
// reused the rig env, so BEADS_DIR/GC_RIG still resolved bd to the rig store
// and missed the HQ session bead. This asserts the returned env is city-scoped:
// BEADS_DIR points at the city .beads (not the rig's), and the rig scope vars
// are cleared — the exact deltas that route bd to the right store. It drives
// the real env builder rather than stubbing (the gcw-kcm4 lesson).
func TestSessionPointerStoreEnvClearsRigScope(t *testing.T) {
	for _, key := range []string{"GC_CITY", "GC_CITY_PATH", "GC_CITY_ROOT"} {
		t.Setenv(key, "")
	}
	// Simulate the hook running under a rig scope, as a crm pool worker does.
	t.Setenv("GC_RIG", "crm")

	city := t.TempDir()
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("name = \"t\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rigStore := filepath.Join(city, "rigs", "crm")
	if err := os.MkdirAll(rigStore, 0o755); err != nil {
		t.Fatal(err)
	}

	cityDir, env, err := sessionPointerStoreEnv(context.Background(), rigStore, "crm/gastown.polecat")
	if err != nil {
		t.Fatalf("sessionPointerStoreEnv(%q): %v", rigStore, err)
	}
	if env == nil {
		t.Fatalf("sessionPointerStoreEnv(%q) skipped — expected a resolved city env", rigStore)
	}

	// cityDir must resolve to the city, not the rig store.
	wantCity, _ := filepath.EvalSymlinks(city)
	gotCity, _ := filepath.EvalSymlinks(cityDir)
	if gotCity != wantCity {
		t.Errorf("cityDir = %q, want city %q", cityDir, city)
	}

	// BEADS_DIR must be the city .beads — NOT the rig store's .beads (the bug).
	if got, bug := env["BEADS_DIR"], filepath.Join(rigStore, ".beads"); got == bug {
		t.Errorf("BEADS_DIR = %q still targets the RIG store — bd would miss the HQ session bead", got)
	}
	if got, want := env["BEADS_DIR"], filepath.Join(cityDir, ".beads"); got != want {
		t.Errorf("BEADS_DIR = %q, want city .beads %q", got, want)
	}
	// The rig scope must be cleared, or bd resolves back to the rig store.
	if env["GC_RIG"] != "" {
		t.Errorf("GC_RIG = %q, want cleared (a rig-scoped env makes bd miss the HQ session bead)", env["GC_RIG"])
	}
	if env["GC_RIG_ROOT"] != "" {
		t.Errorf("GC_RIG_ROOT = %q, want cleared", env["GC_RIG_ROOT"])
	}
	// The claim actor is preserved for authorship on the pointer write.
	if env["BEADS_ACTOR"] != "crm/gastown.polecat" {
		t.Errorf("BEADS_ACTOR = %q, want the claim actor", env["BEADS_ACTOR"])
	}
}

// TestSessionPointerStoreEnvSkipsWhenCityUnresolved guards the caveat-1 fix: if
// no city can be resolved (GC_CITY unset and the work-bead store sits outside
// any city tree), sessionPointerStoreDir falls back to its input; rather than
// silently writing the pointer to that wrong store, the env builder returns a
// nil env so the best-effort write is skipped (no wrong-store write, no
// park-triggering warning).
func TestSessionPointerStoreEnvSkipsWhenCityUnresolved(t *testing.T) {
	for _, key := range []string{"GC_CITY", "GC_CITY_PATH", "GC_CITY_ROOT"} {
		t.Setenv(key, "")
	}
	// A bare dir with no city.toml anywhere above it — city is unresolvable.
	orphan := filepath.Join(t.TempDir(), "some", "work", "dir")
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}

	_, env, err := sessionPointerStoreEnv(context.Background(), orphan, "crm/gastown.polecat")
	if err != nil {
		t.Fatalf("sessionPointerStoreEnv(%q): unexpected error: %v", orphan, err)
	}
	if env != nil {
		t.Errorf("sessionPointerStoreEnv(%q) returned a non-nil env %v — expected a skip (nil) when the city cannot be resolved, to avoid a wrong-store write", orphan, env)
	}
}

// TestIsResolvedCityRootMatchesResolverMarkers guards the city-root guard
// against regressing to a city.toml-only check: the resolver (findCity /
// validateCityPath) accepts a city via city.toml OR the .gc/ runtime root, so a
// valid .gc/-only city (city roots need not carry city.toml) must NOT be
// treated as unresolved and skipped.
func TestIsResolvedCityRootMatchesResolverMarkers(t *testing.T) {
	withCityTOML := t.TempDir()
	if err := os.WriteFile(filepath.Join(withCityTOML, "city.toml"), []byte("name = \"t\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !isResolvedCityRoot(withCityTOML) {
		t.Errorf("isResolvedCityRoot(city.toml dir) = false, want true")
	}

	withRuntimeRoot := t.TempDir() // .gc/ but no city.toml — still a valid city root
	if err := os.MkdirAll(filepath.Join(withRuntimeRoot, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !isResolvedCityRoot(withRuntimeRoot) {
		t.Errorf("isResolvedCityRoot(.gc/-only dir) = false, want true — a .gc/ runtime root is a valid city; skipping it would drop session-pointer writes for city.toml-less cities")
	}

	if isResolvedCityRoot(t.TempDir()) {
		t.Errorf("isResolvedCityRoot(bare dir) = true, want false")
	}
}
