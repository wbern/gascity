package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBdFastpathEnabledDefaultsOnWithExplicitOptOut(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want bool
	}{
		{name: "unset", want: true},
		{name: "enabled", raw: "1", want: true},
		{name: "enabled trimmed", raw: " 1 ", want: true},
		{name: "disabled", raw: "0", want: false},
		{name: "disabled trimmed", raw: " 0 ", want: false},
		{name: "invalid fails closed", raw: "yes", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := bdFastpathEnabled(tc.raw); got != tc.want {
				t.Fatalf("bdFastpathEnabled(%q) = %t, want %t", tc.raw, got, tc.want)
			}
		})
	}
}

func TestTryEarlyBdShimRoutesOptInJSONShow(t *testing.T) {
	t.Setenv("GC_BD_FASTPATH", "1")
	t.Setenv("GC_CITY_PATH", "/tmp/gc2")

	shim := writeEarlyBdShim(t, "printf 'shim:%s\\n' \"$*\"\n")
	previous := earlyBdShimPath
	earlyBdShimPath = func(_ string) (string, error) { return shim, nil }
	t.Cleanup(func() { earlyBdShimPath = previous })

	var stdout, stderr bytes.Buffer
	code, handled := tryEarlyBdShim([]string{"bd", "show", "gcw-123", "--json"}, strings.NewReader(""), &stdout, &stderr)
	if !handled || code != 0 {
		t.Fatalf("tryEarlyBdShim() = (%d, %t), want (0, true); stderr=%q", code, handled, stderr.String())
	}
	if got, want := stdout.String(), "shim:show gcw-123 --json\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestTryEarlyBdShimHonorsExplicitOptOut(t *testing.T) {
	t.Setenv("GC_BD_FASTPATH", "0")
	t.Setenv("GC_CITY_PATH", "/tmp/gc2")

	previous := earlyBdShimPath
	earlyBdShimPath = func(_ string) (string, error) {
		t.Fatal("explicit opt-out must not resolve the bdshim path")
		return "", nil
	}
	t.Cleanup(func() { earlyBdShimPath = previous })

	if code, handled := tryEarlyBdShim([]string{"bd", "show", "gcw-123", "--json"}, strings.NewReader(""), io.Discard, io.Discard); handled || code != 0 {
		t.Fatalf("tryEarlyBdShim() = (%d, %t) when disabled, want (0, false)", code, handled)
	}
}

func TestTryEarlyBdShimPreservesManagedBDEntrypointName(t *testing.T) {
	t.Setenv("GC_BD_FASTPATH", "1")
	t.Setenv("GC_CITY_PATH", "/tmp/gc2")
	managedPath := filepath.Join(t.TempDir(), ".gc", "shimbin", "bd")
	if err := os.MkdirAll(filepath.Dir(managedPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(managedPath, []byte("#!/bin/sh\nprintf '%s\\n' \"${0##*/}\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	previous := earlyBdShimPath
	earlyBdShimPath = func(_ string) (string, error) { return managedPath, nil }
	t.Cleanup(func() { earlyBdShimPath = previous })

	var stdout bytes.Buffer
	code, handled := tryEarlyBdShim([]string{"bd", "show", "gcw-123", "--json"}, strings.NewReader(""), &stdout, io.Discard)
	if !handled || code != 0 {
		t.Fatalf("tryEarlyBdShim() = (%d, %t), want (0, true)", code, handled)
	}
	if got, want := stdout.String(), "bd\n"; got != want {
		t.Fatalf("managed entrypoint name = %q, want %q", got, want)
	}
}

func TestEarlyBdShimBesideGCRequiresExecutableSibling(t *testing.T) {
	dir := t.TempDir()
	gcPath := filepath.Join(dir, "gc")
	if err := os.WriteFile(gcPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := earlyBdShimBesideGC(gcPath); got != "" {
		t.Fatalf("without sibling: got %q, want empty", got)
	}
	shimPath := filepath.Join(dir, "bdshim")
	if err := os.WriteFile(shimPath, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := earlyBdShimBesideGC(gcPath); got != "" {
		t.Fatalf("non-executable sibling: got %q, want empty", got)
	}
	if err := os.Chmod(shimPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := earlyBdShimBesideGC(gcPath); got != shimPath {
		t.Fatalf("executable sibling: got %q, want %q", got, shimPath)
	}
}

func TestEarlyBdShimForCityUsesOnlyTrustedManagedBDAlias(t *testing.T) {
	binDir := t.TempDir()
	gcPath := filepath.Join(binDir, "gc")
	trustedPath := filepath.Join(binDir, "bdshim")
	for _, path := range []string{gcPath, trustedPath} {
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	city := t.TempDir()
	managedDir := filepath.Join(city, ".gc", "shimbin")
	if err := os.MkdirAll(managedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	managedPath := filepath.Join(managedDir, "bd")
	foreignPath := filepath.Join(t.TempDir(), "bdshim")
	if err := os.WriteFile(foreignPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(foreignPath, managedPath); err != nil {
		t.Fatal(err)
	}
	if got := earlyBdShimForCity(gcPath, city); got != "" {
		t.Fatalf("foreign managed entry = %q, want fallback", got)
	}
	if err := os.Remove(managedPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(trustedPath, managedPath); err != nil {
		t.Fatal(err)
	}
	if got := earlyBdShimForCity(gcPath, city); got != managedPath {
		t.Fatalf("trusted managed entry = %q, want %q", got, managedPath)
	}
}

func TestTryEarlyBdShimFailsClosedForUnsafeShapes(t *testing.T) {
	t.Setenv("GC_BD_FASTPATH", "1")
	t.Setenv("GC_CITY_PATH", "/tmp/gc2")

	called := false
	previous := earlyBdShimPath
	earlyBdShimPath = func(_ string) (string, error) {
		called = true
		return "", nil
	}
	t.Cleanup(func() { earlyBdShimPath = previous })

	for _, args := range [][]string{
		{"bd", "show", "gcw-123"},                            // non-JSON output is not yet parity-pinned
		{"bd", "show", "gcw-123", "--json", "--rig", "x"},    // rig scope must remain on full gc bd
		{"--rig", "x", "bd", "show", "gcw-123", "--json"},    // root-level rig scope must remain on full gc bd
		{"bd", "--city", "gc2", "show", "gcw-123", "--json"}, // explicit city uses gc bd's path validation
		{"bd", "list", "--json"},                             // list scope is not necessarily city-wide
		{"bd", "update", "gcw-123", "--status", "closed"},    // writes retain gc bd guards
		{"bd", "show", "gcw-123", "--json", "--verbose"},     // unknown rendering flag
		{"bd", "show", "gcw-123", "--json", "extra"},         // extra positional
		{"bd", "show", "gcw 123", "--json"},                  // malformed bead ID
		{"bd", "show", " gcw-123 ", "--json"},                // do not normalize malformed input
	} {
		if code, handled := tryEarlyBdShim(args, strings.NewReader(""), io.Discard, io.Discard); handled || code != 0 {
			t.Fatalf("tryEarlyBdShim(%v) = (%d, %t), want (0, false)", args, code, handled)
		}
	}
	if called {
		t.Fatal("unsafe shape resolved bdshim path")
	}
}

func TestRunUsesEarlyBdShim(t *testing.T) {
	t.Setenv("GC_BD_FASTPATH", "1")
	t.Setenv("GC_CITY_PATH", "/tmp/gc2")
	shim := writeEarlyBdShim(t, "printf '{\\\"id\\\":\\\"gcw-123\\\"}\\n'\n")
	previous := earlyBdShimPath
	earlyBdShimPath = func(_ string) (string, error) { return shim, nil }
	t.Cleanup(func() { earlyBdShimPath = previous })

	var stdout, stderr bytes.Buffer
	if code := run([]string{"bd", "show", "gcw-123", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run() = %d, want 0; stderr=%q", code, stderr.String())
	}
	if got, want := stdout.String(), "{\"id\":\"gcw-123\"}\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestTryEarlyBdShimFallsBackWithoutManagedCityOrShim(t *testing.T) {
	t.Setenv("GC_BD_FASTPATH", "1")
	t.Setenv("GC_CITY_PATH", "")
	t.Setenv("GC_CITY", "")

	previous := earlyBdShimPath
	earlyBdShimPath = func(_ string) (string, error) {
		t.Fatal("bdshim path must not be resolved without managed city context")
		return "", nil
	}
	t.Cleanup(func() { earlyBdShimPath = previous })

	if code, handled := tryEarlyBdShim([]string{"bd", "show", "gcw-123", "--json"}, strings.NewReader(""), io.Discard, io.Discard); handled || code != 0 {
		t.Fatalf("without city: got (%d, %t), want (0, false)", code, handled)
	}
}

func TestTryEarlyBdShimPreservesShimExitCode(t *testing.T) {
	t.Setenv("GC_BD_FASTPATH", "1")
	t.Setenv("GC_CITY_PATH", "/tmp/gc2")

	shim := writeEarlyBdShim(t, "printf controller-down >&2\nexit 7\n")
	previous := earlyBdShimPath
	earlyBdShimPath = func(_ string) (string, error) { return shim, nil }
	t.Cleanup(func() { earlyBdShimPath = previous })

	var stderr bytes.Buffer
	if code, handled := tryEarlyBdShim([]string{"bd", "show", "gcw-123", "--json"}, strings.NewReader(""), io.Discard, &stderr); !handled || code != 7 {
		t.Fatalf("tryEarlyBdShim() = (%d, %t), want (7, true); stderr=%q", code, handled, stderr.String())
	}
}

func TestTryEarlyBdShimNormalizesSignalExitToFailure(t *testing.T) {
	t.Setenv("GC_BD_FASTPATH", "1")
	t.Setenv("GC_CITY_PATH", "/tmp/gc2")

	shim := writeEarlyBdShim(t, "kill -TERM $$\n")
	previous := earlyBdShimPath
	earlyBdShimPath = func(_ string) (string, error) { return shim, nil }
	t.Cleanup(func() { earlyBdShimPath = previous })

	if code, handled := tryEarlyBdShim([]string{"bd", "show", "gcw-123", "--json"}, strings.NewReader(""), io.Discard, io.Discard); !handled || code != 1 {
		t.Fatalf("tryEarlyBdShim() = (%d, %t), want (1, true)", code, handled)
	}
}

func writeEarlyBdShim(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bdshim")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
