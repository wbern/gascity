package main

import (
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
