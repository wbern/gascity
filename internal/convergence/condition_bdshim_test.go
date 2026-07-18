package convergence

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/citylayout"
)

// writeExecutableBd creates an executable bd stub at dir/bd.
func writeExecutableBd(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %q: %v", dir, err)
	}
	path := filepath.Join(dir, "bd")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
	return path
}

// A gate condition script inherits PATH fronted with the gc-as-bd shim bin dir;
// without GC_BD_REAL the shim refuses and the check fails. Environ() must
// resolve the real bd (excluding the shim dir) so a shimmed `bd` passes through.
func TestConditionEnvInjectsGCBdReal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec-bit semantics differ on windows")
	}
	city := t.TempDir()
	writeExecutableBd(t, citylayout.ShimbinDir(city)) // the shim's bd, fronted first
	realDir := t.TempDir()
	realBd := writeExecutableBd(t, realDir)
	t.Setenv("PATH", citylayout.ShimbinDir(city)+string(os.PathListSeparator)+realDir)

	got := ""
	for _, e := range (ConditionEnv{BeadID: "gc2-1", CityPath: city}).Environ() {
		if v, ok := strings.CutPrefix(e, citylayout.RealBdEnvVar+"="); ok {
			got = v
		}
	}
	if got != realBd {
		t.Fatalf("%s = %q, want the real bd %q", citylayout.RealBdEnvVar, got, realBd)
	}
}

// When no real bd exists outside the shim dir, GC_BD_REAL is omitted (rather
// than pointing at the shim, which would recurse).
func TestConditionEnvOmitsGCBdRealWhenNoRealBd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec-bit semantics differ on windows")
	}
	city := t.TempDir()
	writeExecutableBd(t, citylayout.ShimbinDir(city)) // only the shim's bd
	t.Setenv("PATH", citylayout.ShimbinDir(city))

	for _, e := range (ConditionEnv{CityPath: city}).Environ() {
		if strings.HasPrefix(e, citylayout.RealBdEnvVar+"=") {
			t.Fatalf("expected no %s when only the shim bd is on PATH; got %q", citylayout.RealBdEnvVar, e)
		}
	}
}
